package agent

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows calls golang.org/x/sys/windows does not carry.

var (
	netapi32 = windows.NewLazySystemDLL("netapi32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	userenv  = windows.NewLazySystemDLL("userenv.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")
	wtsapi32 = windows.NewLazySystemDLL("wtsapi32.dll")

	procNetUserAdd              = netapi32.NewProc("NetUserAdd")
	procNetUserDel              = netapi32.NewProc("NetUserDel")
	procNetUserSetInfo          = netapi32.NewProc("NetUserSetInfo")
	procWTSEnumerateProcessesEx = wtsapi32.NewProc("WTSEnumerateProcessesExW")
	procWTSFreeMemoryEx         = wtsapi32.NewProc("WTSFreeMemoryExW")
	procNetLocalGroupAddMembers = netapi32.NewProc("NetLocalGroupAddMembers")
	procLogonUserW              = advapi32.NewProc("LogonUserW")
	procLoadUserProfileW        = userenv.NewProc("LoadUserProfileW")
	procUnloadUserProfile       = userenv.NewProc("UnloadUserProfile")
	procDeleteProfileW          = userenv.NewProc("DeleteProfileW")
	procCreateWindowStationW    = user32.NewProc("CreateWindowStationW")
	procCloseWindowStation      = user32.NewProc("CloseWindowStation")
	procGetProcessWindowStation = user32.NewProc("GetProcessWindowStation")
	procSetProcessWindowStation = user32.NewProc("SetProcessWindowStation")
	procCreateDesktopW          = user32.NewProc("CreateDesktopW")
	procCloseDesktop            = user32.NewProc("CloseDesktop")
)

const (
	nerrSuccess        = 0
	nerrUserExists     = 2224
	nerrUserNotFound   = 2221
	errorMemberInAlias = 1378

	userPrivUser       = 1
	ufScript           = 0x0001
	ufAccountDisable   = 0x0002
	ufPasswdCantChange = 0x0040
	ufNormalAccount    = 0x0200
	ufDontExpirePasswd = 0x10000
	// Every job account's flags, disabled at cleanup.
	accountFlags = ufScript | ufNormalAccount | ufDontExpirePasswd | ufPasswdCantChange

	wtsAnySession        = 0xFFFFFFFE
	wtsTypeProcessLevel1 = 1

	logon32LogonBatch      = 4
	logon32ProviderDefault = 0

	piNoUI = 1

	winstaAllAccess  = 0x37F
	desktopAllAccess = 0x01FF

	jobObjectCPURateControlInformation = 15
	cpuRateControlEnable               = 0x1
	cpuRateControlHardCap              = 0x4

	belowNormalPriorityClass = 0x00004000
	createSuspended          = 0x00000004
	createNoWindow           = 0x08000000
	limitJobMemory           = 0x00000200
	limitPriorityClass       = 0x00000020
	limitDieOnUnhandled      = 0x00000400
)

type userInfo1 struct {
	Name        *uint16
	Password    *uint16
	PasswordAge uint32
	Priv        uint32
	HomeDir     *uint16
	Comment     *uint16
	Flags       uint32
	ScriptPath  *uint16
}

// WTS_PROCESS_INFO_EXW.
type wtsProcessInfoEx struct {
	SessionID          uint32
	ProcessID          uint32
	ProcessName        *uint16
	UserSID            *windows.SID
	NumberOfThreads    uint32
	HandleCount        uint32
	PagefileUsage      uint32
	PeakPagefileUsage  uint32
	WorkingSetSize     uint32
	PeakWorkingSetSize uint32
	UserTime           int64
	KernelTime         int64
}

type localGroupMembersInfo0 struct {
	SID *windows.SID
}

type profileInfo struct {
	Size        uint32
	Flags       uint32
	UserName    *uint16
	ProfilePath *uint16
	DefaultPath *uint16
	ServerName  *uint16
	PolicyPath  *uint16
	Profile     windows.Handle
}

type cpuRateControl struct {
	ControlFlags uint32
	CPURate      uint32
}

func netStatus(r uintptr) error {
	if r == nerrSuccess {
		return nil
	}
	return windows.Errno(r)
}

func netUserAdd(name, password, comment []uint16) error {
	info := userInfo1{Name: &name[0], Password: &password[0], Priv: userPrivUser, Comment: &comment[0],
		Flags: accountFlags}
	r, _, _ := procNetUserAdd.Call(0, 1, uintptr(unsafe.Pointer(&info)), 0)
	return netStatus(r)
}

func netUserDel(name string) error {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return err
	}
	r, _, _ := procNetUserDel.Call(0, uintptr(unsafe.Pointer(n)))
	return netStatus(r)
}

func netLocalGroupAddMember(group string, sid *windows.SID) error {
	g, err := windows.UTF16PtrFromString(group)
	if err != nil {
		return err
	}
	m := localGroupMembersInfo0{SID: sid}
	r, _, _ := procNetLocalGroupAddMembers.Call(0, uintptr(unsafe.Pointer(g)), 0, uintptr(unsafe.Pointer(&m)), 1)
	if r == errorMemberInAlias {
		return nil
	}
	return netStatus(r)
}

func logonUser(name string, password []uint16) (windows.Token, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	dot, _ := windows.UTF16PtrFromString(".")
	var t windows.Token
	r, _, e := procLogonUserW.Call(uintptr(unsafe.Pointer(n)), uintptr(unsafe.Pointer(dot)), uintptr(unsafe.Pointer(&password[0])),
		logon32LogonBatch, logon32ProviderDefault, uintptr(unsafe.Pointer(&t)))
	if r == 0 {
		return 0, e
	}
	return t, nil
}

func loadUserProfile(t windows.Token, name string) (windows.Handle, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	pi := profileInfo{Flags: piNoUI, UserName: n}
	pi.Size = uint32(unsafe.Sizeof(pi))
	r, _, e := procLoadUserProfileW.Call(uintptr(t), uintptr(unsafe.Pointer(&pi)))
	if r == 0 {
		return 0, e
	}
	return pi.Profile, nil
}

func unloadUserProfile(t windows.Token, h windows.Handle) error {
	r, _, e := procUnloadUserProfile.Call(uintptr(t), uintptr(h))
	if r == 0 {
		return e
	}
	return nil
}

func deleteProfile(sid string) error {
	s, err := windows.UTF16PtrFromString(sid)
	if err != nil {
		return err
	}
	r, _, e := procDeleteProfileW.Call(uintptr(unsafe.Pointer(s)), 0, 0)
	if r == 0 {
		return e
	}
	return nil
}

// securityAttributes holds a security descriptor made from SDDL.
func securityAttributes(sddl string) (*windows.SecurityAttributes, error) {
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, err
	}
	sa := &windows.SecurityAttributes{SecurityDescriptor: sd}
	sa.Length = uint32(unsafe.Sizeof(*sa))
	return sa, nil
}

func createWindowStation(name string, sa *windows.SecurityAttributes) (windows.Handle, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	r, _, e := procCreateWindowStationW.Call(uintptr(unsafe.Pointer(n)), 0, winstaAllAccess, uintptr(unsafe.Pointer(sa)))
	if r == 0 {
		return 0, e
	}
	return windows.Handle(r), nil
}

func closeWindowStation(h windows.Handle) { procCloseWindowStation.Call(uintptr(h)) }

func processWindowStation() windows.Handle {
	r, _, _ := procGetProcessWindowStation.Call()
	return windows.Handle(r)
}

func setProcessWindowStation(h windows.Handle) error {
	r, _, e := procSetProcessWindowStation.Call(uintptr(h))
	if r == 0 {
		return e
	}
	return nil
}

func createDesktop(name string, sa *windows.SecurityAttributes) (windows.Handle, error) {
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return 0, err
	}
	r, _, e := procCreateDesktopW.Call(uintptr(unsafe.Pointer(n)), 0, 0, 0, desktopAllAccess, uintptr(unsafe.Pointer(sa)))
	if r == 0 {
		return 0, e
	}
	return windows.Handle(r), nil
}

func closeDesktop(h windows.Handle) { procCloseDesktop.Call(uintptr(h)) }

// netUserSetFlags sets a local account's flags (USER_INFO_1008).
func netUserSetFlags(name []uint16, flags uint32) error {
	r, _, _ := procNetUserSetInfo.Call(0, uintptr(unsafe.Pointer(&name[0])), 1008, uintptr(unsafe.Pointer(&flags)), 0)
	return netStatus(r)
}

// processIDsOf lists every process on the PC signed in as sid, by Windows'
// own reading of each process's account.
func processIDsOf(sid string) ([]uint32, error) {
	level := uint32(wtsTypeProcessLevel1)
	var list *wtsProcessInfoEx
	var count uint32
	r, _, err := procWTSEnumerateProcessesEx.Call(0, uintptr(unsafe.Pointer(&level)), wtsAnySession,
		uintptr(unsafe.Pointer(&list)), uintptr(unsafe.Pointer(&count)))
	if r == 0 {
		return nil, err
	}
	defer procWTSFreeMemoryEx.Call(wtsTypeProcessLevel1, uintptr(unsafe.Pointer(list)), uintptr(count))
	var out []uint32
	for _, p := range unsafe.Slice(list, count) {
		if p.UserSID != nil && p.UserSID.String() == sid {
			out = append(out, p.ProcessID)
		}
	}
	return out, nil
}
