// Package seccomp builds and installs the system-call filter the fence puts
// on every command it runs. The filter narrows which kernel interfaces a
// confined command may use, so it cannot tamper with other processes, leave
// its namespaces, remount the filesystem, load kernel code, reach the kernel
// keyrings, stack a filter of its own over the fence's, or signal the Abhed
// process that launched it.
//
// Command returns the one profile, "command/3", built for one Abhed process
// and its process group. Its Program is classic BPF,
// assembled here in pure Go for amd64 and arm64 from per-architecture
// system-call tables: it first checks the architecture and kills the process
// on any other one (x32 included, on amd64), then matches the call number and
// returns allow or an errno. Refusals are EPERM, except clone3, which gets
// ENOSYS so the C library falls back to clone, whose flags the filter can
// read.
//
// Signals: kill, tkill, tgkill, rt_sigqueueinfo and rt_tgsigqueueinfo naming
// Abhed's process id, and kill of its group or of every process, are refused;
// so is pidfd_send_signal, since the filter cannot see whom a pidfd names.
// Abhed's other thread ids are not known to the filter; Landlock ABI 6 scopes
// signals fully.
//
// Sockets: with the network off, socket() is refused for every family, since
// the filesystem layer only stops TCP and UDP would otherwise get out. With
// it on, only inet, inet6 and route netlink sockets can be created. Unix
// sockets are refused in both modes, so a command cannot reach a host daemon
// such as a container engine, a session bus or an ssh or gpg agent.
// socketpair() is always allowed, so processes can still talk to each other.
//
// What breaks under the filter: debuggers and tracers such as strace and gdb
// (no ptrace); tools that install their own seccomp sandbox, such as a
// headless browser without its sandbox turned off; anything that creates a
// namespace or mounts; and clients of host daemons over unix sockets.
//
// Install is for a re-executed helper about to exec the command: it sets
// no_new_privs and loads the filter on every thread of the process. It cannot
// be undone, and a second Install is refused by the filter itself. Describe
// says what is applied, for doctor and the record.
//
// Install works on Linux only; elsewhere it returns ErrUnsupported. Building
// and describing a Program work everywhere, so the programs can be tested on
// any host.
package seccomp
