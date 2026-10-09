package main

import "syscall"

// restrictUmask makes every file, directory and socket this process creates
// owner-only.
//
// Setting it once at startup is stronger than chmod-ing after the fact. A
// socket is published the instant bind returns, so a chmod that follows leaves
// a window in which the umask's more permissive mode was the real one — and
// for this daemon the socket's mode is the entire access control.
func restrictUmask() { syscall.Umask(0o077) }
