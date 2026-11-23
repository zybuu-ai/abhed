package app

import "sync/atomic"

// guestEnded is told each console guest whose session ended (auth.guest),
// once serve has a server to remove its sessions from.
var guestEnded atomic.Pointer[func(owner string)]
