package agent

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unsafe"

	"github.com/kunchenguid/runner-up/internal/config"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// winHost is Host on Windows, run as SYSTEM.
type winHost struct {
	log        *slog.Logger
	jobsDir    string
	group      string
	powershell string
	// threads is the machine's processor count, which each job's processor
	// cap is a share of.
	threads uint32
	// SetProcessWindowStation applies to the whole service; one job's
	// desktop is made at a time.
	stationMu sync.Mutex
}

const (
	hiddenAccounts = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\Winlogon\SpecialAccounts\UserList`
	profileList    = `SOFTWARE\Microsoft\Windows NT\CurrentVersion\ProfileList`
	usersSID       = "S-1-5-32-545"
	accountComment = "runner-up job account; the runner-up service deletes it after its job"
	// jobGroup holds every job account. runner-up setup lets it sign in only
	// as a batch logon: never at the screen, over Remote Desktop or from the
	// network.
	jobGroup = "runner-up-jobs"
)

func newWinHost(log *slog.Logger) (*winHost, error) {
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = `C:\Windows`
	}
	threads := uint32(windows.GetActiveProcessorCount(windows.ALL_PROCESSOR_GROUPS))
	if threads == 0 {
		return nil, errors.New("no processors counted")
	}
	// Backup and restore open whatever a job account left, whatever its
	// permissions; debug ends a job process that locked SYSTEM out of it.
	if err := enablePrivileges("SeBackupPrivilege", "SeRestorePrivilege", "SeDebugPrivilege",
		"SeAssignPrimaryTokenPrivilege", "SeIncreaseQuotaPrivilege"); err != nil {
		return nil, err
	}
	return &winHost{log: log, jobsDir: filepath.Join(jobsRoot(), "jobs"), group: jobGroup,
		powershell: filepath.Join(root, `System32\WindowsPowerShell\v1.0\powershell.exe`), threads: threads}, nil
}

func enablePrivileges(names ...string) error {
	var t windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_ADJUST_PRIVILEGES|windows.TOKEN_QUERY, &t); err != nil {
		return err
	}
	defer t.Close()
	for _, n := range names {
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(n), &luid); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_ENABLED}
		if err := windows.AdjustTokenPrivileges(t, false, &tp, 0, nil, nil); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	return nil
}

// local qualifies a name with this machine's, so a domain account of the same
// name can never be meant.
func local(name string) (string, error) {
	pc, err := windows.ComputerName()
	if err != nil {
		return "", err
	}
	return pc + `\` + name, nil
}

func (h *winHost) LookupAccount(name string) (string, error) {
	q, err := local(name)
	if err != nil {
		return "", err
	}
	sid, _, _, err := windows.LookupSID("", q)
	if errors.Is(err, windows.ERROR_NONE_MAPPED) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return sid.String(), nil
}

func (h *winHost) CreateAccount(name string, password []uint16) (string, error) {
	n, _ := windows.UTF16FromString(name)
	c, _ := windows.UTF16FromString(accountComment)
	pw := append(append([]uint16{}, password...), 0)
	defer wipe(pw)
	err := netUserAdd(n, pw, c)
	if errors.Is(err, windows.Errno(nerrUserExists)) {
		return "", errAccountExists
	}
	if err != nil {
		return "", err
	}
	sid, err := h.LookupAccount(name)
	if err == nil && sid == "" {
		err = errors.New("made, but not found")
	}
	return sid, err
}

func (h *winHost) folder(a Account) string { return filepath.Join(h.jobsDir, a.Name) }

func (h *winHost) Prepare(a Account, files map[string][]byte) error {
	sid, err := windows.StringToSid(a.SID)
	if err != nil {
		return err
	}
	users, err := windows.StringToSid(usersSID)
	if err != nil {
		return err
	}
	usersName, _, _, err := users.LookupAccount("")
	if err != nil {
		return err
	}
	if err := netLocalGroupAddMember(usersName, sid); err != nil {
		return fmt.Errorf("Users: %w", err)
	}
	if err := netLocalGroupAddMember(h.group, sid); err != nil {
		return fmt.Errorf("%s: %w", h.group, err)
	}
	k, _, err := registry.CreateKey(registry.LOCAL_MACHINE, hiddenAccounts, registry.SET_VALUE)
	if err != nil {
		return err
	}
	err = k.SetDWordValue(a.Name, 0)
	k.Close()
	if err != nil {
		return err
	}
	// Only the account, SYSTEM and Administrators; Modify for the account,
	// and the same for whatever it owns, so it cannot lock SYSTEM out.
	dir := h.folder(a)
	if err := os.Mkdir(dir, 0o700); err != nil {
		return err
	}
	if err := setDACL(dir, fmt.Sprintf("D:P(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)(A;OICI;0x1301bf;;;%s)(A;OICI;0x1301bf;;;OW)", a.SID)); err != nil {
		return err
	}
	for name, b := range files {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o600); err != nil {
			return err
		}
	}
	return nil
}

func setDACL(path, sddl string) error {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil)
}

func protectFile(path string) error { return setDACL(path, "D:P(A;;FA;;;SY)(A;;FA;;;BA)") }

// winJob is a started job and everything it holds.
type winJob struct {
	token     windows.Token
	profile   windows.Handle
	station   windows.Handle
	desktop   windows.Handle
	job       windows.Handle
	process   windows.Handle
	out       *os.File
	stop      sync.Once
	closeOnce sync.Once
	log       *slog.Logger
}

func (h *winHost) Start(a Account, password []uint16, input []byte, share config.Share) (r Running, err error) {
	j := &winJob{log: h.log.With("account", a.Name)}
	defer func() {
		if err != nil {
			j.Close()
		}
	}()
	pw := append(append([]uint16{}, password...), 0)
	j.token, err = logonUser(a.Name, pw)
	wipe(pw)
	if err != nil {
		return nil, fmt.Errorf("sign-in: %w", err)
	}
	if err := withoutHostControlPrivileges(j.token); err != nil {
		return nil, fmt.Errorf("host-control rights: %w", err)
	}
	if j.profile, err = loadUserProfile(j.token, a.Name); err != nil {
		return nil, fmt.Errorf("profile: %w", err)
	}
	var env *uint16
	if err := windows.CreateEnvironmentBlock(&env, j.token, false); err != nil {
		return nil, fmt.Errorf("environment: %w", err)
	}
	defer windows.DestroyEnvironmentBlock(env)

	if err := h.makeDesktop(j, a); err != nil {
		return nil, fmt.Errorf("desktop: %w", err)
	}
	if j.job, err = h.makeJobObject(share); err != nil {
		return nil, fmt.Errorf("job object: %w", err)
	}

	// The child's ends of its standard input and output are the only
	// handles it inherits.
	inherit := &windows.SecurityAttributes{InheritHandle: 1}
	inherit.Length = uint32(unsafe.Sizeof(*inherit))
	var childIn, parentIn, parentOut, childOut windows.Handle
	if err := windows.CreatePipe(&childIn, &parentIn, inherit, 0); err != nil {
		return nil, err
	}
	defer windows.CloseHandle(childIn)
	if err := windows.CreatePipe(&parentOut, &childOut, inherit, 0); err != nil {
		windows.CloseHandle(parentIn)
		return nil, err
	}
	defer windows.CloseHandle(childOut)
	windows.SetHandleInformation(parentIn, windows.HANDLE_FLAG_INHERIT, 0)
	windows.SetHandleInformation(parentOut, windows.HANDLE_FLAG_INHERIT, 0)
	j.out = os.NewFile(uintptr(parentOut), "job output")
	stdin := os.NewFile(uintptr(parentIn), "job input")

	attrs, err := windows.NewProcThreadAttributeList(1)
	if err != nil {
		stdin.Close()
		return nil, err
	}
	defer attrs.Delete()
	handles := []windows.Handle{childIn, childOut}
	if err := attrs.Update(windows.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])); err != nil {
		stdin.Close()
		return nil, err
	}
	desktop, _ := windows.UTF16PtrFromString(stationName(a) + `\default`)
	si := windows.StartupInfoEx{ProcThreadAttributeList: attrs.List()}
	si.Cb = uint32(unsafe.Sizeof(si))
	si.Desktop = desktop
	si.Flags = windows.STARTF_USESTDHANDLES
	si.StdInput, si.StdOutput, si.StdErr = childIn, childOut, childOut

	dir := h.folder(a)
	app, _ := windows.UTF16PtrFromString(h.powershell)
	cmd, _ := windows.UTF16PtrFromString(jobCommandLine(h.powershell, dir))
	cwd, _ := windows.UTF16PtrFromString(dir)
	var pi windows.ProcessInformation
	flags := uint32(createSuspended | createNoWindow | windows.CREATE_UNICODE_ENVIRONMENT | windows.EXTENDED_STARTUPINFO_PRESENT)
	if err := windows.CreateProcessAsUser(j.token, app, cmd, nil, nil, true, flags, env, cwd, &si.StartupInfo, &pi); err != nil {
		stdin.Close()
		return nil, fmt.Errorf("start: %w", err)
	}
	j.process = pi.Process
	defer windows.CloseHandle(pi.Thread)
	if err := windows.AssignProcessToJobObject(j.job, pi.Process); err != nil {
		windows.TerminateProcess(pi.Process, 1)
		stdin.Close()
		return nil, fmt.Errorf("contain: %w", err)
	}
	if _, err := windows.ResumeThread(pi.Thread); err != nil {
		stdin.Close()
		return nil, err
	}
	go func() {
		stdin.Write(input)
		stdin.Close()
	}()
	return j, nil
}

// Jobs must not shut down or restart the machine, or change its time zone.
// Windows gives Users shutdown and time-zone rights on its desktop editions,
// and no privilege can be denied through account policy.
var hostControlPrivileges = []string{
	"SeShutdownPrivilege",
	"SeRemoteShutdownPrivilege",
	"SeTimeZonePrivilege",
}

// withoutHostControlPrivileges deletes these rights from the job's token and
// checks they are gone. A deleted privilege cannot be enabled again, and
// every token copied from this one lacks it too: the job's processes, and
// those Windows' services start for it, such as WMI's. Task Scheduler signs a
// job account in afresh for no task: it refuses one that would (S4U), and an
// interactive-only one never runs.
func withoutHostControlPrivileges(t windows.Token) error {
	removed := map[windows.LUID]string{}
	for _, n := range hostControlPrivileges {
		var luid windows.LUID
		if err := windows.LookupPrivilegeValue(nil, windows.StringToUTF16Ptr(n), &luid); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		// Removing one the token never held is not an error.
		tp := windows.Tokenprivileges{PrivilegeCount: 1}
		tp.Privileges[0] = windows.LUIDAndAttributes{Luid: luid, Attributes: windows.SE_PRIVILEGE_REMOVED}
		if err := windows.AdjustTokenPrivileges(t, false, &tp, 0, nil, nil); err != nil {
			return fmt.Errorf("%s: %w", n, err)
		}
		removed[luid] = n
	}
	held, err := tokenPrivileges(t)
	if err != nil {
		return err
	}
	for _, p := range held {
		if n, ok := removed[p.Luid]; ok {
			return fmt.Errorf("the token still holds %s", n)
		}
	}
	return nil
}

func tokenPrivileges(t windows.Token) ([]windows.LUIDAndAttributes, error) {
	var n uint32
	windows.GetTokenInformation(t, windows.TokenPrivileges, nil, 0, &n)
	if n == 0 {
		return nil, errors.New("the token's privileges could not be read")
	}
	b := make([]byte, n)
	if err := windows.GetTokenInformation(t, windows.TokenPrivileges, &b[0], n, &n); err != nil {
		return nil, err
	}
	return (*windows.Tokenprivileges)(unsafe.Pointer(&b[0])).AllPrivileges(), nil
}

func stationName(a Account) string { return "runner-up-" + a.Name }

// makeDesktop gives the job a window station and desktop of its own, which
// only it, SYSTEM and Administrators may use: a window can open there, and
// nothing reaches the machine's own desktop or another job's.
func (h *winHost) makeDesktop(j *winJob, a Account) error {
	sa, err := securityAttributes(fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;BA)(A;;GA;;;%s)", a.SID))
	if err != nil {
		return err
	}
	h.stationMu.Lock()
	defer h.stationMu.Unlock()
	if j.station, err = createWindowStation(stationName(a), sa); err != nil {
		return err
	}
	old := processWindowStation()
	if err := setProcessWindowStation(j.station); err != nil {
		return err
	}
	j.desktop, err = createDesktop("default", sa)
	if rerr := setProcessWindowStation(old); rerr != nil && err == nil {
		err = rerr
	}
	return err
}

// makeJobObject contains a job to its share: a hard cap of share.Cores of the
// machine's processors and share.MemoryMB for all its processes together.
func (h *winHost) makeJobObject(share config.Share) (windows.Handle, error) {
	rate := uint32(share.Cores) * 10000 / h.threads
	if rate < 1 || rate > 10000 {
		rate = 10000
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return 0, err
	}
	// Every process in it ends when the service closes it, none may leave
	// it, and together they run below normal priority within their memory.
	// It sets no window limits: Chromium's sandbox, Electron's included,
	// cannot start a page while any is set. The job's windows,
	// clipboard and atoms belong to its own window station, which only it
	// may open.
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | limitJobMemory | limitPriorityClass | limitDieOnUnhandled
	limits.BasicLimitInformation.PriorityClass = belowNormalPriorityClass
	limits.JobMemoryLimit = uintptr(uint64(share.MemoryMB) << 20)
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("limits: %w", err)
	}
	cap := cpuRateControl{ControlFlags: cpuRateControlEnable | cpuRateControlHardCap, CPURate: rate}
	if _, err := windows.SetInformationJobObject(job, jobObjectCPURateControlInformation,
		uintptr(unsafe.Pointer(&cap)), uint32(unsafe.Sizeof(cap))); err != nil {
		windows.CloseHandle(job)
		return 0, fmt.Errorf("processor cap: %w", err)
	}
	return job, nil
}

func (j *winJob) Output() io.Reader { return j.out }

func (j *winJob) Wait() (int, error) {
	if _, err := windows.WaitForSingleObject(j.process, windows.INFINITE); err != nil {
		return 0, err
	}
	var code uint32
	if err := windows.GetExitCodeProcess(j.process, &code); err != nil {
		return 0, err
	}
	return int(code), nil
}

func (j *winJob) Stop() {
	if j.job == 0 {
		return
	}
	j.stop.Do(func() {
		if err := windows.TerminateJobObject(j.job, 1); err != nil {
			j.log.Warn("could not end the job's processes", "err", err)
		}
	})
}

func (j *winJob) Close() {
	j.closeOnce.Do(func() {
		j.Stop()
		if j.job != 0 {
			windows.CloseHandle(j.job)
		}
		if j.process != 0 {
			windows.CloseHandle(j.process)
		}
		if j.out != nil {
			j.out.Close()
		}
		if j.profile != 0 {
			// Processes that have just ended may still hold the profile's
			// registry for a moment.
			for i := 0; i < 10; i++ {
				if err := unloadUserProfile(j.token, j.profile); err == nil {
					break
				} else if i == 9 {
					j.log.Warn("could not unload the profile", "err", err)
				}
				time.Sleep(time.Second)
			}
		}
		if j.token != 0 {
			j.token.Close()
		}
		if j.desktop != 0 {
			closeDesktop(j.desktop)
		}
		if j.station != 0 {
			closeWindowStation(j.station)
		}
	})
}

// processesOf lists the processes signed in as sid, opened to be ended.
// Windows' own list names each process's account, so a process cannot hide
// by locking its token; one SYSTEM cannot open is an error.
// A process started after the list was taken has only reused a listed
// number, and is left alone.
func processesOf(sid string) ([]windows.Handle, error) {
	var listed windows.Filetime
	windows.GetSystemTimeAsFileTime(&listed)
	ids, err := processIDsOf(sid)
	if err != nil {
		return nil, err
	}
	var out []windows.Handle
	var errs []error
	for _, id := range ids {
		p, err := windows.OpenProcess(windows.PROCESS_TERMINATE|windows.SYNCHRONIZE|windows.PROCESS_QUERY_LIMITED_INFORMATION, false, id)
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			continue // it has ended
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("process %d: %w", id, err))
			continue
		}
		var created, exited, kernel, user windows.Filetime
		if err := windows.GetProcessTimes(p, &created, &exited, &kernel, &user); err != nil {
			windows.CloseHandle(p)
			errs = append(errs, fmt.Errorf("process %d: %w", id, err))
			continue
		}
		if created.Nanoseconds() > listed.Nanoseconds() {
			windows.CloseHandle(p)
			continue
		}
		out = append(out, p)
	}
	return out, errors.Join(errs...)
}

// EndProcesses ends every process signed in as the account, inside its job
// or not (a process it started through Windows' own services is not).
func (h *winHost) EndProcesses(a Account) error {
	for round := 0; round < 10; round++ {
		ps, err := processesOf(a.SID)
		for _, p := range ps {
			windows.TerminateProcess(p, 1)
			windows.WaitForSingleObject(p, 5000)
			windows.CloseHandle(p)
		}
		if err != nil {
			return err
		}
		if len(ps) == 0 {
			return nil
		}
	}
	return errors.New("its processes keep coming back")
}

// DisableAccount stops anything signing in as the account again, a
// scheduled task or another service on its behalf included, before its
// processes are ended.
func (h *winHost) DisableAccount(a Account) error {
	sid, err := h.LookupAccount(a.Name)
	if err != nil || sid == "" {
		return err
	}
	if sid != a.SID {
		return fmt.Errorf("%s now has another SID, %s; leaving it", a.Name, sid)
	}
	n, _ := windows.UTF16FromString(a.Name)
	err = netUserSetFlags(n, accountFlags|ufAccountDisable)
	if errors.Is(err, windows.Errno(nerrUserNotFound)) {
		return nil
	}
	return err
}

// runPowerShell runs script as the service, with env added, and fails on any
// error the script stops on.
func (h *winHost) runPowerShell(script string, env ...string) error {
	script = "$ErrorActionPreference = 'Stop'; $ProgressPreference = 'SilentlyContinue'\n" + script
	u := utf16.Encode([]rune(script))
	b := make([]byte, 2*len(u))
	for i, c := range u {
		b[2*i], b[2*i+1] = byte(c), byte(c>>8)
	}
	cmd := exec.Command(h.powershell, "-NoLogo", "-NoProfile", "-NonInteractive", "-ExecutionPolicy", "Bypass",
		"-EncodedCommand", base64.StdEncoding.EncodeToString(b))
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// RemoveTasks deletes every scheduled task that would run as the account.
func (h *winHost) RemoveTasks(a Account) error {
	return h.runPowerShell(`$sid = $env:RUNNER_UP_JOB_SID; $name = $env:RUNNER_UP_JOB_NAME
$tasks = @(Get-ScheduledTask | Where-Object { $u = [string]$_.Principal.UserId; $u -eq $sid -or $u -eq $name -or $u.EndsWith('\' + $name) })
$tasks | Unregister-ScheduledTask -Confirm:$false
`, "RUNNER_UP_JOB_SID="+a.SID, "RUNNER_UP_JOB_NAME="+a.Name)
}

// The folders every account on the machine may write in. Only C:\'s own entries
// are read: anything deeper there belongs to a folder below.
func sharedFolders() []struct {
	path string
	deep bool
} {
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	root := os.Getenv("SystemRoot")
	if root == "" {
		root = drive + `\Windows`
	}
	data := os.Getenv("ProgramData")
	if data == "" {
		data = drive + `\ProgramData`
	}
	out := []struct {
		path string
		deep bool
	}{{drive + `\`, false}, {data, true}, {drive + `\Users\Public`, true}}
	for _, p := range []string{`Temp`, `Tasks`, `tracing`, `Registration\CRMLog`,
		`System32\Tasks`, `System32\spool\drivers\color`, `System32\Microsoft\Crypto\RSA\MachineKeys`,
		`System32\FxsTmp`, `System32\com\dmp`, `SysWOW64\Tasks`, `SysWOW64\FxsTmp`, `SysWOW64\com\dmp`} {
		out = append(out, struct {
			path string
			deep bool
		}{filepath.Join(root, p), true})
	}
	return out
}

// RemoveSharedFiles deletes what the account owns in the shared folders.
// It never follows a link: a link the account made is deleted itself.
func (h *winHost) RemoveSharedFiles(a Account) error {
	sid, err := windows.StringToSid(a.SID)
	if err != nil {
		return err
	}
	var errs []error
	for _, f := range sharedFolders() {
		root, err := openRoot(f.path)
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			continue
		}
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if err := removeOwned(root, sid, f.deep); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", f.path, err))
		}
		root.close()
	}
	return errors.Join(errs...)
}

// open opens path itself, a link included, for access, with backup rights so
// SYSTEM gets in whatever the path's own permissions say.
func open(path string, access uint32) (windows.Handle, error) {
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return windows.CreateFile(p, access, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, nil,
		windows.OPEN_EXISTING, windows.FILE_FLAG_BACKUP_SEMANTICS|windows.FILE_FLAG_OPEN_REPARSE_POINT, 0)
}

func (h *winHost) profilePath(sid string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileList+`\`+sid, registry.QUERY_VALUE)
	if err != nil {
		return ""
	}
	defer k.Close()
	p, _, err := k.GetStringValue("ProfileImagePath")
	if err != nil {
		return ""
	}
	if x, err := registry.ExpandString(p); err == nil {
		p = x
	}
	return p
}

// profileFolders are the names in the Users folder the account's profile
// may have: its own, and the one Windows recorded.
func (h *winHost) profileFolders(a Account) []string {
	out := []string{a.Name}
	if p := h.profilePath(a.SID); p != "" && strings.EqualFold(filepath.Dir(p), h.usersDir()) &&
		strings.HasPrefix(strings.ToLower(filepath.Base(p)), strings.ToLower(a.Name)) && !strings.EqualFold(filepath.Base(p), a.Name) {
		out = append(out, filepath.Base(p))
	}
	return out
}

// DeleteProfile deletes the account's profile: Windows' own way first, then
// by hand for whatever that left.
func (h *winHost) DeleteProfile(a Account) error {
	folders := h.profileFolders(a)
	var err error
	for i := 0; i < 5; i++ {
		err = deleteProfile(a.SID)
		if err == nil || errors.Is(err, windows.ERROR_FILE_NOT_FOUND) || errors.Is(err, windows.ERROR_PATH_NOT_FOUND) {
			err = nil
			break
		}
		time.Sleep(2 * time.Second)
	}
	if err != nil {
		h.log.Warn("Windows did not delete the profile; deleting it by hand", "account", a.Name, "err", err)
	}
	var errs []error
	for _, name := range folders {
		if err := removeAt(h.usersDir(), name); err != nil {
			errs = append(errs, err)
		}
	}
	if rerr := registry.DeleteKey(registry.LOCAL_MACHINE, profileList+`\`+a.SID); rerr != nil && !errors.Is(rerr, windows.ERROR_FILE_NOT_FOUND) {
		errs = append(errs, rerr)
	}
	return errors.Join(errs...)
}

func (h *winHost) usersDir() string {
	drive := os.Getenv("SystemDrive")
	if drive == "" {
		drive = "C:"
	}
	return drive + `\Users`
}

func (h *winHost) RemoveFolder(a Account) error { return removeAt(h.jobsDir, a.Name) }

func (h *winHost) DeleteAccount(a Account) error {
	// Only the account this record names: the same name with another SID
	// is someone else's.
	sid, err := h.LookupAccount(a.Name)
	if err != nil {
		return err
	}
	if sid != "" && sid != a.SID {
		return fmt.Errorf("%s now has another SID, %s; leaving it", a.Name, sid)
	}
	if sid != "" {
		if err := netUserDel(a.Name); err != nil && !errors.Is(err, windows.Errno(nerrUserNotFound)) {
			return err
		}
	}
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, hiddenAccounts, registry.SET_VALUE)
	if err == nil {
		err = k.DeleteValue(a.Name)
		k.Close()
		if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
			err = nil
		}
	} else if errors.Is(err, windows.ERROR_FILE_NOT_FOUND) {
		err = nil
	}
	return err
}

func (h *winHost) Verify(a Account) error {
	var left []string
	if sid, err := h.LookupAccount(a.Name); err != nil || sid == a.SID {
		left = append(left, "the account")
	}
	if ps, err := processesOf(a.SID); err != nil || len(ps) > 0 {
		for _, p := range ps {
			windows.CloseHandle(p)
		}
		left = append(left, "processes")
	}
	if existsAt(h.jobsDir, a.Name) {
		left = append(left, "its job folder")
	}
	for _, name := range h.profileFolders(a) {
		if existsAt(h.usersDir(), name) {
			left = append(left, "its profile folder "+name)
		}
	}
	if k, err := registry.OpenKey(registry.LOCAL_MACHINE, profileList+`\`+a.SID, registry.QUERY_VALUE); err == nil {
		k.Close()
		left = append(left, "its profile record")
	}
	if len(left) > 0 {
		return fmt.Errorf("left behind: %s", strings.Join(left, ", "))
	}
	return nil
}
