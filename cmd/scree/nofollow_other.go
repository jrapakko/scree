//go:build !unix

package main

// noFollow is 0 where open has no O_NOFOLLOW; callers Lstat first.
const noFollow = 0
