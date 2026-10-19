package seccomp

import (
	"fmt"
	"strconv"
)

// callNames maps the system calls the profile names to their numbers'
// names on arch, for naming the calls a listener is sent.
func callNames(arch string) map[uint32]string {
	out := map[uint32]string{}
	for name, nr := range arches[arch].nr {
		out[nr] = name
	}
	return out
}

// socketFamilies names the address families a refused socket is likeliest to ask for.
var socketFamilies = map[uint64]string{1: "unix", 2: "inet", 10: "inet6", 16: "netlink", 17: "packet"}

// refusalName is how a refused call is recorded: its name, and for socket
// and socketpair the family asked for.
func refusalName(name string, args [6]uint64) string {
	if name != "socket" && name != "socketpair" {
		return name
	}
	f, ok := socketFamilies[args[0]&0xffffffff]
	if !ok {
		f = strconv.FormatUint(args[0]&0xffffffff, 10)
	}
	return fmt.Sprintf("%s(%s)", name, f)
}
