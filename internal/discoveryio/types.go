package discoveryio

// Entry is a child directory entry. Kind is file, directory, symlink or other.
type Entry struct {
	Name string
	Kind string
}

// Identity identifies a file before and after a read.
type Identity struct {
	Device uint64
	Inode  uint64
	Size   int64
	Mtime  int64
	Ctime  int64
}

// File holds bounded bytes and their identity.
type File struct {
	Data   []byte
	Before Identity
	After  Identity
}
