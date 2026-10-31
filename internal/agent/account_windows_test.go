package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// releasedGrant is what runner-up's service has granted the runner account
// on its pipe in every release so far. A newer agent must fit it to reach a
// service an older runner-up setup installed.
const releasedGrant = 0x12008b

// Intent (a bug found on a real PC): the agent asked for more of the
// service's pipe than the service grants the runner account, so Windows
// refused every runner account that is not an administrator with "Access is
// denied", and no account place could take a job. An account holding only
// the runner account's grant reaches the service the way the agent does.
func TestAnAccountGrantedOnlyTheRunnersRightsReachesTheService(t *testing.T) {
	user, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatal(err)
	}
	me := user.User.Sid
	grant := runnerGrant(t, me)
	if grant != releasedGrant {
		t.Errorf("the service grants the runner account %#x; released services grant %#x, which an agent must still fit", grant, releasedGrant)
	}
	// A pipe like the service's, owned by this test's account and open to it
	// alone, with only the runner account's grant. With no SYSTEM or
	// Administrators entry, an administrator running the test holds no more
	// than a standard runner account does.
	path := fmt.Sprintf(`\\.\pipe\runner-up-test-%d-%d`, os.Getpid(), time.Now().UnixNano())
	sa, err := securityAttributes(fmt.Sprintf("O:%sD:P(A;;%#x;;;%s)", me, grant, me))
	if err != nil {
		t.Fatal(err)
	}
	name, _ := windows.UTF16PtrFromString(path)
	pipe, err := windows.CreateNamedPipe(name, windows.PIPE_ACCESS_DUPLEX|windows.FILE_FLAG_FIRST_PIPE_INSTANCE,
		windows.PIPE_TYPE_BYTE|windows.PIPE_READMODE_BYTE|windows.PIPE_WAIT|windows.PIPE_REJECT_REMOTE_CLIENTS,
		windows.PIPE_UNLIMITED_INSTANCES, 64<<10, 64<<10, 0, sa)
	if err != nil {
		t.Fatal(err)
	}
	served := make(chan struct{})
	serving := false
	defer func() {
		if !serving {
			windows.CloseHandle(pipe)
			return
		}
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("the service kept the pipe after the agent closed it")
		}
	}()

	// What the agent asked for before, GENERIC_WRITE among it: Windows
	// refuses it here, so this pipe lets in no more than the grant.
	h, err := windows.CreateFile(name, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.READ_CONTROL, 0, nil,
		windows.OPEN_EXISTING, windows.SECURITY_SQOS_PRESENT|windows.SECURITY_IDENTIFICATION, 0)
	if err == nil {
		windows.CloseHandle(h)
		t.Fatal("the test's pipe let in more than the runner account's grant, so it proves nothing")
	}
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		t.Fatalf("opening with more than the grant gave %v, want access denied", err)
	}

	conn, err := dialPipe(path, me)
	if err != nil {
		t.Fatalf("an account granted only the runner account's rights could not reach the service: %v", err)
	}
	if err := windows.ConnectNamedPipe(pipe, nil); err != nil && !errors.Is(err, windows.ERROR_PIPE_CONNECTED) {
		conn.Close()
		t.Fatal(err)
	}
	serving = true
	go func() {
		defer close(served)
		handle(context.Background(), pipe, newTestServer(t, newFakeHost()), slog.New(slog.NewTextHandler(io.Discard, nil)))
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := askService(ctx, conn, JobRequest{Version: jobRequestVersion, Op: "probe"}, &out); err != nil {
		t.Fatalf("the probe over the pipe failed: %v\n%s", err, out.String())
	}
	if !strings.HasPrefix(out.String(), serviceReady) {
		t.Errorf("the service answered %q, want %q", out.String(), serviceReady)
	}
}

// runnerGrant is the access the service's pipe gives the runner account sid.
func runnerGrant(t *testing.T, sid *windows.SID) windows.ACCESS_MASK {
	t.Helper()
	sd, err := windows.SecurityDescriptorFromString(pipeSDDL(sid.String()))
	if err != nil {
		t.Fatal(err)
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		t.Fatal(err)
	}
	for i := uint32(0); i < uint32(dacl.AceCount); i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, i, &ace); err != nil {
			t.Fatal(err)
		}
		if (*windows.SID)(unsafe.Pointer(&ace.SidStart)).Equals(sid) {
			return ace.Mask
		}
	}
	t.Fatalf("the service's pipe gives the runner account nothing: %s", pipeSDDL(sid.String()))
	return 0
}
