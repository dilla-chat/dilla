//go:build windows

package desktoplaunch

import (
	"bytes"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

type initialCreatedObject struct {
	file      *os.File
	relative  string
	directory bool
}

// InitialRootClaim owns only objects it atomically created. It never adopts an
// existing root/child and rollback never unlinks a pathname after releasing its
// identity. Native callers derive root from compiled identity and KnownFolders.
type InitialRootClaim struct {
	parent            *DirectoryGuard
	objects           []*initialCreatedObject
	entries           map[string]*initialCreatedObject
	seed              *initialCreatedObject
	layout            *InitialCopyLayout
	seedTemporaryName string
}

func ClaimInitialProgramRoot(path string, layout *InitialCopyLayout) (*InitialRootClaim, error) {
	if layout == nil {
		return nil, errors.New("unprepared initial copy layout")
	}
	return claimInitialRoot(path, layout)
}
func ClaimInitialUpdateRoot(path string) (*InitialRootClaim, error) {
	return claimInitialRoot(path, nil)
}
func claimInitialRoot(path string, layout *InitialCopyLayout) (claim *InitialRootClaim, err error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, errors.New("invalid initial root claim")
	}
	parent, err := PinOwnedDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	claim = &InitialRootClaim{parent: parent, entries: map[string]*initialCreatedObject{}, layout: layout}
	rootParent := parent.guard.handles[len(parent.guard.handles)-1]
	object, err := createInitialObject(rootParent, filepath.Base(path), path, true)
	if object != nil {
		object.relative = "."
		claim.objects = append(claim.objects, object)
		claim.entries["."] = object
	}
	if err != nil {
		return nil, errors.Join(err, claim.Rollback())
	}
	return claim, nil
}

func (c *InitialRootClaim) Close() error {
	if c == nil {
		return nil
	}
	var result error
	for i := len(c.objects) - 1; i >= 0; i-- {
		if c.objects[i].file != nil {
			result = errors.Join(result, c.objects[i].file.Close())
			c.objects[i].file = nil
		}
	}
	if c.parent != nil {
		result = errors.Join(result, c.parent.Close())
		c.parent = nil
	}
	return result
}

func (c *InitialRootClaim) Rollback() error {
	if c == nil {
		return nil
	}
	var result error
	// Entries are registered parent-before-child. Each disposition acts on the
	// still-open created object; unknown children make directory deletion fail.
	for i := len(c.objects) - 1; i >= 0; i-- {
		object := c.objects[i]
		if object.file == nil {
			continue
		}
		disposition := byte(1)
		result = errors.Join(result, windows.SetFileInformationByHandle(windows.Handle(object.file.Fd()), windows.FileDispositionInfo, &disposition, 1))
		result = errors.Join(result, object.file.Close())
		object.file = nil
	}
	if c.parent != nil {
		result = errors.Join(result, c.parent.Close())
		c.parent = nil
	}
	return result
}

func (c *InitialRootClaim) create(relative string, directory bool) (*initialCreatedObject, error) {
	if c.entries[relative] != nil {
		return nil, errors.New("invalid or existing initial claim entry")
	}
	if c.layout != nil {
		if directory {
			if !c.layout.directories[relative] {
				return nil, errors.New("unbound initial directory")
			}
		} else if _, found := c.layout.expectedFile(relative); !found {
			return nil, errors.New("unbound initial file")
		}
	} else if directory || relative != c.seedTemporaryName || relative == "" {
		return nil, errors.New("unbound initial seed staging entry")
	}
	parentRelative := filepath.ToSlash(filepath.Dir(filepath.FromSlash(relative)))
	parent := c.entries[parentRelative]
	if parent == nil || !parent.directory || parent.file == nil {
		return nil, errors.New("missing claimed initial parent")
	}
	object, err := createInitialObject(parent.file, filepath.Base(filepath.FromSlash(relative)), filepath.Join(c.entries["."].file.Name(), filepath.FromSlash(relative)), directory)
	if object != nil {
		object.relative = relative
		c.objects = append(c.objects, object)
		c.entries[relative] = object
	}
	return object, err
}

func (c *InitialRootClaim) CopyVerifiedFile(relative string, source io.Reader) error {
	file, found := c.layout.expectedFile(relative)
	if !found {
		return errors.New("unbound initial copy target")
	}
	return c.copyVerifiedFile(relative, source, file.Size, file.SHA512)
}
func (c *InitialRootClaim) copyVerifiedFile(relative string, source io.Reader, size int64, digest string) error {
	if size < 0 || size > maxFileSize {
		return errors.New("invalid initial copy entry")
	}
	expected, err := decodeDigest(digest)
	if err != nil {
		return err
	}
	parts := strings.Split(relative, "/")
	for i := 1; i < len(parts); i++ {
		name := strings.Join(parts[:i], "/")
		if c.entries[name] == nil {
			if _, err = c.create(name, true); err != nil {
				return err
			}
		}
	}
	object, err := c.create(relative, false)
	if err != nil {
		return err
	}
	hash := sha512.New()
	count, err := io.Copy(io.MultiWriter(object.file, hash), io.LimitReader(source, size+1))
	if err != nil {
		return err
	}
	if count != size || !bytes.Equal(hash.Sum(nil), expected) {
		return errors.New("initial source copy digest or size mismatch")
	}
	if err = object.file.Sync(); err != nil {
		return err
	}
	if _, err = object.file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	hash.Reset()
	count, err = io.Copy(hash, io.LimitReader(object.file, size+1))
	if err != nil {
		return err
	}
	if count != size || !bytes.Equal(hash.Sum(nil), expected) {
		return errors.New("initial copied bytes failed readback")
	}
	return nil
}

func (c *InitialRootClaim) VerifyLayout() error {
	if len(c.entries) > maxDocument+16 {
		return errors.New("initial layout exceeds manifest bound")
	}
	if c.layout != nil && len(c.entries) != len(c.layout.files)+len(c.layout.directories) {
		return errors.New("initial package copy is incomplete")
	}
	seen := map[string]bool{}
	for relative, object := range c.entries {
		if !object.directory {
			continue
		}
		if object.file == nil {
			return errors.New("closed initial root claim")
		}
		if strings.Count(relative, "/") > 128 {
			return errors.New("initial layout depth exceeds manifest bound")
		}
		err := readInitialDirectory(object.file, func(entry os.DirEntry) error {
			child := entry.Name()
			if relative != "." {
				child = relative + "/" + child
			}
			expected := c.entries[child]
			if len(seen) >= len(c.entries)-1 || seen[child] || expected == nil || expected.file == nil || expected.directory != entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
				return errors.New("unexpected child in initial root claim")
			}
			opened, err := openInitialClaimMetadata(object.file, entry.Name(), expected.directory)
			if err != nil {
				return err
			}
			var actual, held syscall.ByHandleFileInformation
			actualErr := syscall.GetFileInformationByHandle(syscall.Handle(opened.Fd()), &actual)
			heldErr := syscall.GetFileInformationByHandle(syscall.Handle(expected.file.Fd()), &held)
			closeErr := opened.Close()
			if err = errors.Join(actualErr, heldErr, closeErr); err != nil {
				return err
			}
			if actual.VolumeSerialNumber != held.VolumeSerialNumber || actual.FileIndexHigh != held.FileIndexHigh || actual.FileIndexLow != held.FileIndexLow || actual.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || (actual.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0) != expected.directory || !expected.directory && actual.NumberOfLinks != 1 {
				return errors.New("initial destination identity changed")
			}
			seen[child] = true
			return nil
		})
		if err != nil {
			return err
		}
	}
	if len(seen) != len(c.entries)-1 {
		return errors.New("created initial entry is missing")
	}
	return nil
}

// PublishInitialSeed returns attempted=true immediately once the native rename
// was invoked, including error returns. Only preparation failures return false.
func (c *InitialRootClaim) PublishInitialSeed(seed []byte) (attempted bool, err error) {
	if c.layout != nil || len(seed) < 1 || len(seed) > 4096 || c.seed != nil {
		return false, errors.New("invalid initial seed publication")
	}
	var nonce [16]byte
	if _, err = rand.Read(nonce[:]); err != nil {
		return false, err
	}
	temporary := ".initial-seed-" + hex.EncodeToString(nonce[:]) + ".tmp"
	c.seedTemporaryName = temporary
	sum := sha512.Sum512(seed)
	if err = c.copyVerifiedFile(temporary, bytes.NewReader(seed), int64(len(seed)), base64.StdEncoding.EncodeToString(sum[:])); err != nil {
		return false, err
	}
	c.seed = c.entries[temporary]
	err = renameInitialClaim(c.seed.file, c.entries["."].file, "windows-active-0.json")
	if err == nil {
		delete(c.entries, temporary)
		c.seed.relative = "windows-active-0.json"
		c.entries[c.seed.relative] = c.seed
	}
	return true, err
}

// ObserveInitialSeed opens only metadata relative to the retained claimed root;
// bytes are reread through the original held seed handle, never a reopened path.
func (c *InitialRootClaim) ObserveInitialSeed(expected []byte) (exact, absent bool, err error) {
	if c.seed == nil || c.seed.file == nil || c.entries["."] == nil || c.entries["."].file == nil || len(expected) == 0 {
		return false, false, errors.New("unprepared initial seed observation")
	}
	file, err := openInitialClaimMetadata(c.entries["."].file, "windows-active-0.json", false)
	if err != nil {
		if os.IsNotExist(err) {
			return false, true, nil
		}
		return false, false, err
	}
	defer func() { err = errors.Join(err, file.Close()) }()
	actualInfo, err := file.Stat()
	if err != nil {
		return false, false, err
	}
	heldInfo, err := c.seed.file.Stat()
	if err != nil {
		return false, false, err
	}
	if !os.SameFile(actualInfo, heldInfo) {
		return false, false, nil
	}
	if _, err = c.seed.file.Seek(0, io.SeekStart); err != nil {
		return false, false, err
	}
	raw, err := io.ReadAll(io.LimitReader(c.seed.file, int64(len(expected))+1))
	if err != nil {
		return false, false, err
	}
	return bytes.Equal(raw, expected), false, nil
}

func (c *InitialRootClaim) FlushInitialSeed() error {
	if c.seed == nil || c.seed.file == nil {
		return errors.New("unprepared initial seed flush")
	}
	return c.seed.file.Sync()
}

func initialNTError(err error) error {
	if status, ok := err.(windows.NTStatus); ok {
		return status.Errno()
	}
	return err
}

func createInitialObject(parent *os.File, name, path string, directory bool) (object *initialCreatedObject, err error) {
	if (!validProgramPath(name) && (!fixedManifestName.MatchString(name) || directory)) || strings.Contains(name, "/") {
		return nil, errors.New("invalid initial claim component")
	}
	token, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	user, err := token.GetTokenUser()
	_ = token.Close()
	if err != nil {
		return nil, err
	}
	sid := user.User.Sid.String()
	descriptor, err := windows.SecurityDescriptorFromString("O:" + sid + "D:P(A;OICI;FA;;;" + sid + ")(A;OICI;FA;;;SY)(A;OICI;FA;;;BA)")
	if err != nil {
		return nil, err
	}
	nativeName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: nativeName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE, SecurityDescriptor: descriptor}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	options := uint32(windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_NON_DIRECTORY_FILE)
	share := uint32(windows.FILE_SHARE_DELETE)
	if directory {
		options = windows.FILE_SYNCHRONOUS_IO_NONALERT | windows.FILE_OPEN_REPARSE_POINT | windows.FILE_DIRECTORY_FILE
		share = windows.FILE_SHARE_READ | windows.FILE_SHARE_WRITE
	}
	err = windows.NtCreateFile(&handle, windows.GENERIC_READ|windows.GENERIC_WRITE|windows.DELETE, &attributes, &status, nil, windows.FILE_ATTRIBUTE_NORMAL, share, windows.FILE_CREATE, options, 0, 0)
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, initialNTError(err)
	}
	if handle == 0 || handle == windows.InvalidHandle {
		return nil, errors.New("invalid initial created handle")
	}
	if status.Status != 0 || status.Information != 2 {
		_ = windows.CloseHandle(handle)
		return nil, errors.New("initial claim did not create a new object")
	}
	object = &initialCreatedObject{file: os.NewFile(uintptr(handle), path), directory: directory}
	var identity syscall.ByHandleFileInformation
	if err = syscall.GetFileInformationByHandle(syscall.Handle(handle), &identity); err != nil {
		return object, err
	}
	if (identity.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY != 0) != directory || identity.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT != 0 || !directory && identity.NumberOfLinks != 1 {
		return object, errors.New("invalid newly created initial object")
	}
	if err = checkHandleSecurity(syscall.Handle(handle)); err != nil {
		return object, err
	}
	if err = verifyInitialCreatedDescriptor(handle, user.User.Sid, descriptor); err != nil {
		return object, err
	}
	return object, checkNoNamedDataStreams(path, directory)
}

func verifyInitialCreatedDescriptor(handle windows.Handle, user *windows.SID, expected *windows.SECURITY_DESCRIPTOR) error {
	actual, err := windows.GetSecurityInfo(handle, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION|windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		return err
	}
	owner, _, err := actual.Owner()
	if err != nil || owner == nil || !owner.Equals(user) {
		return errors.New("new initial root owner mismatch")
	}
	control, _, err := actual.Control()
	if err != nil || control&windows.SE_DACL_PROTECTED == 0 {
		return errors.New("new initial root DACL is not protected")
	}
	canonical := func(value string) string {
		if len(value) == 0 || len(value) > 65536 {
			return ""
		}
		_, dacl, found := strings.Cut(value, "D:")
		if !found {
			return ""
		}
		if strings.HasPrefix(dacl, "PAI(") {
			return dacl[3:]
		}
		if strings.HasPrefix(dacl, "P(") {
			return dacl[1:]
		}
		return ""
	}
	a, b := canonical(actual.String()), canonical(expected.String())
	if a == "" || b == "" || a != b {
		return errors.New("new initial root DACL differs from requested security")
	}
	var flags uint32
	if err = windows.GetVolumeInformationByHandle(handle, nil, 0, nil, nil, &flags, nil, 0); err != nil {
		return err
	}
	if flags&8 == 0 {
		return errors.New("initial root filesystem does not persist ACLs")
	}
	return nil
}

func openInitialClaimMetadata(parent *os.File, name string, directory bool) (*os.File, error) {
	nativeName, err := windows.NewNTUnicodeString(name)
	if err != nil {
		return nil, err
	}
	attributes := windows.OBJECT_ATTRIBUTES{RootDirectory: windows.Handle(parent.Fd()), ObjectName: nativeName, Attributes: windows.OBJ_CASE_INSENSITIVE | windows.OBJ_DONT_REPARSE}
	attributes.Length = uint32(unsafe.Sizeof(attributes))
	var handle windows.Handle
	var status windows.IO_STATUS_BLOCK
	options := uint32(windows.FILE_NON_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT)
	if directory {
		options = windows.FILE_DIRECTORY_FILE | windows.FILE_OPEN_REPARSE_POINT
	}
	err = windows.NtCreateFile(&handle, windows.FILE_READ_ATTRIBUTES|windows.READ_CONTROL, &attributes, &status, nil, 0, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE, windows.FILE_OPEN, options, 0, 0)
	if err != nil {
		if handle != 0 {
			_ = windows.CloseHandle(handle)
		}
		return nil, initialNTError(err)
	}
	if handle == 0 || handle == windows.InvalidHandle {
		return nil, errors.New("invalid initial observation handle")
	}
	return os.NewFile(uintptr(handle), name), nil
}

//nolint:gosec // G103: fixed FILE_RENAME_INFORMATION contains only a retained parent handle and bounded fixed initial seed name.
func renameInitialClaim(file, parent *os.File, name string) error {
	type renameInformation struct {
		Replace uint32
		Root    windows.Handle
		Length  uint32
		Name    [1]uint16
	}
	var layout renameInformation
	nativeName, err := windows.UTF16FromString(name)
	if err != nil {
		return err
	}
	length := (len(nativeName) - 1) * 2
	buffer := make([]byte, int(unsafe.Offsetof(layout.Name))+length)
	*(*windows.Handle)(unsafe.Pointer(&buffer[unsafe.Offsetof(layout.Root)])) = windows.Handle(parent.Fd())
	binary.LittleEndian.PutUint32(buffer[unsafe.Offsetof(layout.Length):], uint32(length))
	for i, value := range nativeName[:len(nativeName)-1] {
		binary.LittleEndian.PutUint16(buffer[int(unsafe.Offsetof(layout.Name))+i*2:], value)
	}
	var status windows.IO_STATUS_BLOCK
	return initialNTError(windows.NtSetInformationFile(windows.Handle(file.Fd()), &status, &buffer[0], uint32(len(buffer)), windows.FileRenameInformation))
}
