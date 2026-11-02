package loopport

import "syscall"

// Ephemeral is the range the system gives a listener that asks for no port
// (net.inet.ip.portrange.first and .last), 49152 to 65535 by default.
func Ephemeral() (lo, hi int) {
	f, err1 := syscall.SysctlUint32("net.inet.ip.portrange.first")
	l, err2 := syscall.SysctlUint32("net.inet.ip.portrange.last")
	if err1 != nil || err2 != nil || f == 0 || l < f || l > 65535 {
		return 49152, 65535
	}
	return int(f), int(l)
}
