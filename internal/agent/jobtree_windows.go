package agent

import (
	"encoding/binary"
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Deleting what a job account left, as SYSTEM, without ever being led
// somewhere else. A live process of another job's account may swap any
// folder it can write for a junction at any moment, so nothing here goes
// by path below a trusted root: every entry is opened relative to its
// parent's open handle, as the entry itself (a link is opened, never
// followed), with backup intent so the account's own permissions cannot
// lock SYSTEM out, and is deleted through that handle.

const (
	fileFullDirectoryInfo        = 14
	fileFullDirectoryRestartInfo = 15
	// Where FILE_FULL_DIR_INFO keeps its fields.
	dirInfoAttributes = 56
	dirInfoNameLength = 60
	dirInfoName       = 68
)

// entry is an open file or folder, never a link's target.
type entry struct {
	h     windows.Handle
	attrs uint32
	// held: someone holds it without sharing, so it was opened only to read
	// its owner.
	held bool
}

func (e entry) Dir() bool {
	return e.attrs&windows.FILE_ATTRIBUTE_DIRECTORY != 0 && e.attrs&windows.FILE_ATTRIBUTE_REPARSE_POINT == 0
}

func (e entry) close() { windows.CloseHandle(e.h) }

const treeAccess = windows.DELETE | windows.READ_CONTROL | windows.FILE_LIST_DIRECTORY | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE

// openRoot opens a trusted folder, such as C:\ProgramData, by its path. It
// refuses one that is a link.
func openRoot(path string) (entry, error) {
	h, err := open(path, treeAccess&^windows.DELETE)
	if err != nil {
		return entry{}, err
	}
	e, err := describe(h)
	if err == nil && !e.Dir() {
		err = fmt.Errorf("%s is not a plain folder", path)
	}
	if err != nil {
		windows.CloseHandle(h)
		return entry{}, err
	}
	return e, nil
}

// openAt opens name inside the open folder parent: the entry itself, a
// link included.
func openAt(parent windows.Handle, name string) (entry, error) {
	u, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return entry{}, err
	}
	oa := windows.OBJECT_ATTRIBUTES{RootDirectory: parent, ObjectName: u, Attributes: windows.OBJ_CASE_INSENSITIVE}
	oa.Length = uint32(unsafe.Sizeof(oa))
	var h windows.Handle
	ntOpen := func(access uint32) error {
		var iosb windows.IO_STATUS_BLOCK
		return windows.NtCreateFile(&h, access, &oa, &iosb, nil, 0,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN,
			windows.FILE_OPEN_REPARSE_POINT|windows.FILE_OPEN_FOR_BACKUP_INTENT|windows.FILE_SYNCHRONOUS_IO_NONALERT, 0, 0)
	}
	held := false
	err = ntOpen(treeAccess)
	if errors.Is(err, windows.STATUS_ACCESS_DENIED) {
		// Windows' own protected folders, such as ProgramData's Windows
		// Defender, refuse even SYSTEM the right to delete. It is still
		// opened to read its owner and list it, and deleting it then fails
		// as an error.
		err = ntOpen(treeAccess &^ windows.DELETE)
	}
	if errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
		// Someone holds it without sharing reading or deleting: it is still
		// opened to read its owner, and deleting it then fails as an error.
		held = true
		err = ntOpen(windows.READ_CONTROL | windows.FILE_READ_ATTRIBUTES | windows.SYNCHRONIZE)
	}
	if err != nil {
		return entry{}, err
	}
	e, err := describe(h)
	if err != nil {
		windows.CloseHandle(h)
		return entry{}, err
	}
	e.held = held
	return e, nil
}

func describe(h windows.Handle) (entry, error) {
	var info windows.ByHandleFileInformation
	if err := windows.GetFileInformationByHandle(h, &info); err != nil {
		return entry{}, err
	}
	return entry{h: h, attrs: info.FileAttributes}, nil
}

// gone reports an entry that vanished between listing and opening it.
func gone(err error) bool {
	return errors.Is(err, windows.STATUS_OBJECT_NAME_NOT_FOUND) || errors.Is(err, windows.STATUS_OBJECT_PATH_NOT_FOUND) ||
		errors.Is(err, windows.STATUS_DELETE_PENDING)
}

// names lists an open folder's entries.
func names(dir windows.Handle) ([]string, error) {
	var out []string
	buf := make([]byte, 64<<10)
	class := uint32(fileFullDirectoryRestartInfo)
	for {
		err := windows.GetFileInformationByHandleEx(dir, class, &buf[0], uint32(len(buf)))
		class = fileFullDirectoryInfo
		if errors.Is(err, windows.ERROR_NO_MORE_FILES) {
			return out, nil
		}
		if err != nil {
			return out, err
		}
		for off := 0; ; {
			rec := buf[off:]
			n := int(binary.LittleEndian.Uint32(rec[dirInfoNameLength:])) / 2
			u := make([]uint16, n)
			for i := range u {
				u[i] = binary.LittleEndian.Uint16(rec[dirInfoName+2*i:])
			}
			if name := windows.UTF16ToString(u); name != "." && name != ".." {
				out = append(out, name)
			}
			next := int(binary.LittleEndian.Uint32(rec))
			if next == 0 {
				break
			}
			off += next
		}
	}
}

func ownerIs(h windows.Handle, sid *windows.SID) (bool, error) {
	sd, err := windows.GetSecurityInfo(h, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return false, err
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return false, err
	}
	return owner.Equals(sid), nil
}

// deleteOpen deletes an open entry, a folder once empty, at once from its
// folder even while someone else holds it open.
func deleteOpen(h windows.Handle) error {
	flags := uint32(windows.FILE_DISPOSITION_DELETE | windows.FILE_DISPOSITION_POSIX_SEMANTICS | windows.FILE_DISPOSITION_IGNORE_READONLY_ATTRIBUTE)
	return windows.SetFileInformationByHandle(h, windows.FileDispositionInfoEx, (*byte)(unsafe.Pointer(&flags)), 4)
}

// removeTree deletes an open entry and everything below it. A link inside
// is deleted itself.
func removeTree(e entry) error {
	var errs []error
	if e.Dir() {
		children, err := names(e.h)
		if err != nil {
			errs = append(errs, err)
		}
		for _, name := range children {
			c, err := openAt(e.h, name)
			if gone(err) {
				continue
			}
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
				continue
			}
			if err := removeTree(c); err != nil {
				errs = append(errs, fmt.Errorf("%s\\%w", name, err))
			}
			c.close()
		}
	}
	if len(errs) == 0 {
		if err := deleteOpen(e.h); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// removeAt deletes name inside the trusted folder root, if it is there.
func removeAt(root, name string) error {
	r, err := openRoot(root)
	if err != nil {
		return err
	}
	defer r.close()
	e, err := openAt(r.h, name)
	if gone(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer e.close()
	return removeTree(e)
}

// existsAt reports whether name is inside the trusted folder root.
func existsAt(root, name string) bool {
	r, err := openRoot(root)
	if err != nil {
		return false
	}
	defer r.close()
	e, err := openAt(r.h, name)
	if err != nil {
		return !gone(err)
	}
	e.close()
	return true
}

// removeOwned deletes every entry sid owns in the open folder dir, and with
// deep in every plain folder below it, never through a link.
func removeOwned(dir entry, sid *windows.SID, deep bool) error {
	children, err := names(dir.h)
	var errs []error
	if err != nil {
		errs = append(errs, err)
	}
	for _, name := range children {
		c, err := openAt(dir.h, name)
		if gone(err) || errors.Is(err, windows.STATUS_SHARING_VIOLATION) {
			// Not even its owner can be read, as with C:\pagefile.sys: only
			// Windows holds a file that way, and the job's processes have
			// ended.
			continue
		}
		if errors.Is(err, windows.STATUS_ACCESS_DENIED) {
			// Refused to SYSTEM even to read with backup rights: only a
			// driver of Windows' own guards a folder that way, and no job
			// account can write there either.
			continue
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", name, err))
			continue
		}
		owned, err := ownerIs(c.h, sid)
		switch {
		case err != nil:
		case owned:
			if err := removeTree(c); err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", name, err))
			}
		case deep && c.Dir() && !c.held:
			if err := removeOwned(c, sid, true); err != nil {
				errs = append(errs, fmt.Errorf("%s\\%w", name, err))
			}
		}
		c.close()
	}
	return errors.Join(errs...)
}
