package archive

import (
	"bytes"
	"errors"
	"unsafe"

	"golang.org/x/sys/unix"
)

const xattrPAXPrefix = "SCHILY.xattr."

// xattrArgs is struct xattr_args of the Linux UAPI (https://github.com/torvalds/linux/blob/v7.0/include/uapi/linux/xattr.h).
type xattrArgs struct {
	value uint64
	size  uint32
	flags uint32
}

// setxattrat calls the setxattrat system call of Linux 6.13.
func setxattrat(dirfd int, path string, atFlags int, name string, value []byte) error {
	pathPointer, err := unix.BytePtrFromString(path)
	if err != nil {
		return err
	}
	namePointer, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	args := xattrArgs{size: uint32(len(value))}
	if len(value) > 0 {
		args.value = uint64(uintptr(unsafe.Pointer(&value[0])))
	}
	_, _, errno := unix.Syscall6(
		unix.SYS_SETXATTRAT,
		uintptr(dirfd),
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(atFlags),
		uintptr(unsafe.Pointer(namePointer)),
		uintptr(unsafe.Pointer(&args)),
		unsafe.Sizeof(args),
	)
	if errno != 0 {
		return errno
	}
	return nil
}

// listxattrat calls the listxattrat system call of Linux 6.13.
func listxattrat(dirfd int, path string, atFlags int, list []byte) (int, error) {
	pathPointer, err := unix.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	var listPointer unsafe.Pointer
	if len(list) > 0 {
		listPointer = unsafe.Pointer(&list[0])
	}
	size, _, errno := unix.Syscall6(unix.SYS_LISTXATTRAT, uintptr(dirfd), uintptr(unsafe.Pointer(pathPointer)), uintptr(atFlags), uintptr(listPointer), uintptr(len(list)), 0)
	if errno != 0 {
		return 0, errno
	}
	return int(size), nil
}

// getxattrat calls the getxattrat system call of Linux 6.13.
func getxattrat(dirfd int, path string, atFlags int, name string, value []byte) (int, error) {
	pathPointer, err := unix.BytePtrFromString(path)
	if err != nil {
		return 0, err
	}
	namePointer, err := unix.BytePtrFromString(name)
	if err != nil {
		return 0, err
	}
	args := xattrArgs{size: uint32(len(value))}
	if len(value) > 0 {
		args.value = uint64(uintptr(unsafe.Pointer(&value[0])))
	}
	size, _, errno := unix.Syscall6(
		unix.SYS_GETXATTRAT,
		uintptr(dirfd),
		uintptr(unsafe.Pointer(pathPointer)),
		uintptr(atFlags),
		uintptr(unsafe.Pointer(namePointer)),
		uintptr(unsafe.Pointer(&args)),
		unsafe.Sizeof(args),
	)
	if errno != 0 {
		return 0, errno
	}
	return int(size), nil
}

// readXattrs returns the extended attributes that list and get read as PAX records, or nil if there are none or the file system does not support them.
// list and get have the semantics of the listxattr and getxattr system calls.
func readXattrs(list func(names []byte) (int, error), get func(name string, value []byte) (int, error)) (map[string]string, error) {
	var stackNames [256]byte
	names := stackNames[:]
	for {
		size, err := list(names)
		switch {
		case errors.Is(err, unix.ENOTSUP) || err == nil && size == 0:
			return nil, nil
		case errors.Is(err, unix.ERANGE):
			if size, err = list(nil); err != nil {
				return nil, err
			}
			names = make([]byte, size)
			continue
		case err != nil:
			return nil, err
		}
		names = names[:size]
		break
	}
	records := map[string]string{}
	for name := range bytes.SplitSeq(bytes.TrimSuffix(names, []byte{0}), []byte{0}) {
		value, err := readXattr(string(name), get)
		if errors.Is(err, unix.ENODATA) {
			continue
		}
		if err != nil {
			return nil, err
		}
		records[xattrPAXPrefix+string(name)] = string(value)
	}
	return records, nil
}

func readXattr(name string, get func(name string, value []byte) (int, error)) ([]byte, error) {
	for {
		size, err := get(name, nil)
		if err != nil {
			return nil, err
		}
		value := make([]byte, size)
		size, err = get(name, value)
		if errors.Is(err, unix.ERANGE) {
			continue
		}
		if err != nil {
			return nil, err
		}
		return value[:size], nil
	}
}
