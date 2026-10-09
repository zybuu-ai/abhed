import Foundation

/// A refusal to run, with the exit status it ends the helper with.
struct HelperError: Error, CustomStringConvertible {
    let description: String
    let status: Int32

    init(_ description: String, status: Int32 = ExitStatus.refused) {
        self.description = description
        self.status = status
    }
}

/// The exit statuses of helper protocol 1.
enum ExitStatus {
    /// The VM stopped: the guest powered off, or the helper stopped it
    /// because its standard input ended or it was asked to terminate.
    static let stopped: Int32 = 0
    /// The VM could not start, or stopped with an error.
    static let failed: Int32 = 1
    /// The command line or the configuration was refused; no VM ran.
    static let refused: Int32 = 2
}

struct Disk {
    let path: String
    let readOnly: Bool
}

struct GuestPort {
    let port: UInt32
    let socket: String
}

/// The file `run --config` reads (helperConfig in driver/microvm/helper.go).
/// An unknown key is refused, so a setting the helper lacks is never ignored.
struct VMConfig {
    let kernel: String
    let bootArgs: String
    let cpus: Int
    let memoryMiB: Int
    let disks: [Disk]
    let vsockSocket: String
    let guestPorts: [GuestPort]

    static let protocolVersion = 1
    // sockaddr_un holds 104 bytes with the terminating NUL.
    static let maxSocketPath = 103
    static let maxDisks = 4
    static let maxGuestPorts = 16

    static func load(path: String) throws -> VMConfig {
        guard let data = FileManager.default.contents(atPath: path) else {
            throw HelperError("cannot read the configuration \(path)")
        }
        let obj: Any
        do {
            obj = try JSONSerialization.jsonObject(with: data)
        } catch {
            throw HelperError("the configuration is not JSON: \(error.localizedDescription)")
        }
        guard let top = obj as? [String: Any] else {
            throw HelperError("the configuration is not a JSON object")
        }
        return try parse(top)
    }

    static func parse(_ top: [String: Any]) throws -> VMConfig {
        try onlyKeys(top, ["protocol", "kernel", "boot_args", "cpus", "memory_mib", "disks",
                           "vsock_socket", "guest_ports", "network"], in: "the configuration")
        let proto = try int(top, "protocol")
        guard proto == protocolVersion else {
            throw HelperError("the configuration is for helper protocol \(proto); this helper speaks \(protocolVersion)")
        }
        // The VM never has a network device; a configuration asking for one is refused.
        guard try string(top, "network") == "none" else {
            throw HelperError("network must be \"none\": this helper never gives a VM a network device")
        }
        let kernel = try file(top, "kernel")
        let bootArgs = try string(top, "boot_args")
        let cpus = try int(top, "cpus")
        let memoryMiB = try int(top, "memory_mib")

        guard let rawDisks = top["disks"] as? [Any], !rawDisks.isEmpty, rawDisks.count <= maxDisks else {
            throw HelperError("disks must be a list of 1 to \(maxDisks) disks")
        }
        var disks: [Disk] = []
        for (i, raw) in rawDisks.enumerated() {
            guard let d = raw as? [String: Any] else { throw HelperError("disks[\(i)] is not an object") }
            try onlyKeys(d, ["path", "read_only"], in: "disks[\(i)]")
            disks.append(Disk(path: try file(d, "path"), readOnly: try bool(d, "read_only", default: false)))
        }

        let sock = try socketPath(top, "vsock_socket")
        var ports: [GuestPort] = []
        if let raw = top["guest_ports"] {
            guard let list = raw as? [Any], list.count <= maxGuestPorts else {
                throw HelperError("guest_ports must be a list of at most \(maxGuestPorts) ports")
            }
            for (i, item) in list.enumerated() {
                guard let p = item as? [String: Any] else { throw HelperError("guest_ports[\(i)] is not an object") }
                try onlyKeys(p, ["port", "socket"], in: "guest_ports[\(i)]")
                let n = try int(p, "port")
                guard n > 0, n <= Int(UInt32.max) else { throw HelperError("guest_ports[\(i)].port \(n) is not a vsock port") }
                guard !ports.contains(where: { $0.port == UInt32(n) }) else {
                    throw HelperError("guest port \(n) is listed twice")
                }
                ports.append(GuestPort(port: UInt32(n), socket: try socketPath(p, "socket")))
            }
        }
        return VMConfig(kernel: kernel, bootArgs: bootArgs, cpus: cpus, memoryMiB: memoryMiB,
                        disks: disks, vsockSocket: sock, guestPorts: ports)
    }

    private static func onlyKeys(_ d: [String: Any], _ known: Set<String>, in what: String) throws {
        for k in d.keys.sorted() where !known.contains(k) {
            throw HelperError("\(what) has the key \"\(k)\", which this helper does not know")
        }
    }

    private static func isBool(_ v: Any) -> Bool {
        guard let n = v as? NSNumber else { return false }
        return CFGetTypeID(n) == CFBooleanGetTypeID()
    }

    private static func int(_ d: [String: Any], _ key: String) throws -> Int {
        guard let v = d[key] else { throw HelperError("\(key) is missing") }
        guard let n = v as? NSNumber, !isBool(n), let i = Int(exactly: n.doubleValue), Double(i) == n.doubleValue else {
            throw HelperError("\(key) must be a whole number")
        }
        return i
    }

    private static func bool(_ d: [String: Any], _ key: String, default def: Bool) throws -> Bool {
        guard let v = d[key] else { return def }
        guard isBool(v), let b = v as? Bool else { throw HelperError("\(key) must be true or false") }
        return b
    }

    private static func string(_ d: [String: Any], _ key: String) throws -> String {
        guard let v = d[key] else { throw HelperError("\(key) is missing") }
        guard let s = v as? String else { throw HelperError("\(key) must be a string") }
        return s
    }

    private static func absolute(_ d: [String: Any], _ key: String) throws -> String {
        let p = try string(d, key)
        guard p.hasPrefix("/"), !p.contains("\0") else { throw HelperError("\(key) must be an absolute path, not \"\(p)\"") }
        return p
    }

    /// A regular file, the kernel or a disk image.
    private static func file(_ d: [String: Any], _ key: String) throws -> String {
        let p = try absolute(d, key)
        var st = stat()
        guard stat(p, &st) == 0 else { throw HelperError("\(key) \(p): \(String(cString: strerror(errno)))") }
        guard (st.st_mode & S_IFMT) == S_IFREG else { throw HelperError("\(key) \(p) is not a regular file") }
        return p
    }

    private static func socketPath(_ d: [String: Any], _ key: String) throws -> String {
        let p = try absolute(d, key)
        guard p.utf8.count <= maxSocketPath else {
            throw HelperError("\(key) \(p) is longer than a Unix socket path may be (\(maxSocketPath) bytes)")
        }
        return p
    }
}
