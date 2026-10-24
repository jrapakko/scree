//go:build !unix

package main

// Non-Unix systems have no umask; store.Open still chmods the index files.
