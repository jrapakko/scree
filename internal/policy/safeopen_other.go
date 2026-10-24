//go:build !unix

package policy

// No O_NOFOLLOW or O_NONBLOCK here: the Lstat before opening and the
// SameFile check after it refuse a symlink or a swapped file.
const (
	repoOpenFlags = 0
	userOpenFlags = 0
)

func noFollowErr(err error) error { return stripPath(err) }
