// Package mountns gives a fenced command a mount namespace of its own, in
// which paths inside the writable workspace are held read-only or hidden.
// Landlock grants a folder and everything under it, so it cannot keep part of
// a writable folder read-only; a read-only bind mount over that part can.
//
// The launcher is started in a new user and mount namespace (Attr), holding
// only CAP_SYS_ADMIN there. It applies a Plan before Landlock and seccomp,
// then drops every capability (Drop):
//
//	Pin       folders bound onto themselves, so they cannot be renamed or removed
//	ReadOnly  files and folders bound read-only onto themselves
//	Empty     folders covered by an empty, throwaway tmpfs
//
// A file held read-only must have one name, and a folder held read-only no
// file named also outside it: a hard link elsewhere would stay writable, so
// Apply refuses it.
//
// The same files can be mounted at another path too (an ostree host's
// /sysroot, a bind mount). Apply reads the namespace's mounts and applies
// the plan at each such path as well, and refuses one it cannot check.
//
// Every path is opened beneath the plan's root without following a symbolic
// link, and mounted through its descriptor, so a link a command left in the
// workspace cannot move a mount elsewhere.
//
// Where an ordinary user cannot make a user namespace, or gets no capability
// in one (a zero user.max_user_namespaces, Ubuntu's AppArmor restriction),
// Check says why, and the fence refuses the surfaces that need a plan.
package mountns
