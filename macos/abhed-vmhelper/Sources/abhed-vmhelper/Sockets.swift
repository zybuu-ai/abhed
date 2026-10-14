import Foundation
import Virtualization

func errnoText() -> String { String(cString: strerror(errno)) }

private func withSockaddr<T>(_ path: String, _ body: (UnsafePointer<sockaddr>, socklen_t) -> T) -> T {
    var addr = sockaddr_un()
    addr.sun_family = sa_family_t(AF_UNIX)
    let bytes = Array(path.utf8)
    withUnsafeMutableBytes(of: &addr.sun_path) { raw in
        raw.copyBytes(from: bytes.prefix(raw.count - 1))
    }
    addr.sun_len = UInt8(MemoryLayout<sockaddr_un>.size)
    return withUnsafePointer(to: &addr) {
        $0.withMemoryRebound(to: sockaddr.self, capacity: 1) { body($0, socklen_t(MemoryLayout<sockaddr_un>.size)) }
    }
}

private func unixSocket() throws -> Int32 {
    let fd = socket(AF_UNIX, SOCK_STREAM, 0)
    guard fd >= 0 else { throw HelperError("socket: \(errnoText())", status: ExitStatus.failed) }
    _ = fcntl(fd, F_SETFD, FD_CLOEXEC)
    var one: Int32 = 1
    _ = setsockopt(fd, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
    return fd
}

/// listenUnix listens on path, a socket only this user may connect to. An
/// existing file there is never replaced.
func listenUnix(_ path: String) throws -> Int32 {
    let fd = try unixSocket()
    let old = umask(0o077)
    let rc = withSockaddr(path) { bind(fd, $0, $1) }
    umask(old)
    guard rc == 0 else {
        let msg = errnoText()
        close(fd)
        throw HelperError("listening on \(path): \(msg)", status: ExitStatus.failed)
    }
    guard chmod(path, 0o600) == 0, listen(fd, 16) == 0 else {
        let msg = errnoText()
        close(fd)
        unlink(path)
        throw HelperError("listening on \(path): \(msg)", status: ExitStatus.failed)
    }
    return fd
}

func connectUnix(_ path: String) -> Int32 {
    guard let fd = try? unixSocket() else { return -1 }
    let rc = withSockaddr(path) { connect(fd, $0, $1) }
    if rc != 0 {
        close(fd)
        return -1
    }
    return fd
}

func writeAll(_ fd: Int32, _ buf: UnsafeRawPointer, _ n: Int) -> Bool {
    var off = 0
    while off < n {
        let w = write(fd, buf + off, n - off)
        if w < 0 && errno == EINTR { continue }
        if w <= 0 { return false }
        off += w
    }
    return true
}

func writeString(_ fd: Int32, _ s: String) -> Bool {
    let b = Array(s.utf8)
    return b.withUnsafeBytes { writeAll(fd, $0.baseAddress!, $0.count) }
}

/// readHandshake reads "CONNECT <port>\n" one byte at a time, so nothing
/// after the line is consumed, within a few seconds.
func readHandshake(_ fd: Int32) -> UInt32? {
    var tv = timeval(tv_sec: 5, tv_usec: 0)
    _ = setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &tv, socklen_t(MemoryLayout<timeval>.size))
    defer {
        var zero = timeval(tv_sec: 0, tv_usec: 0)
        _ = setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &zero, socklen_t(MemoryLayout<timeval>.size))
    }
    var line: [UInt8] = []
    var b: UInt8 = 0
    while line.count < 64 {
        let n = read(fd, &b, 1)
        if n < 0 && errno == EINTR { continue }
        if n != 1 { return nil }
        if b == UInt8(ascii: "\n") {
            guard let s = String(bytes: line, encoding: .ascii), s.hasPrefix("CONNECT "),
                  let port = UInt32(s.dropFirst("CONNECT ".count)) else { return nil }
            return port
        }
        line.append(b)
    }
    return nil
}

/// Bridge copies both ways between a Unix socket and a vsock connection until
/// both directions end. The vsock connection owns its descriptor and closes it.
final class Bridge {
    private let unixFD: Int32
    private let vsock: VZVirtioSocketConnection
    private let lock = NSLock()
    private var open = 2

    init(unixFD: Int32, vsock: VZVirtioSocketConnection) {
        self.unixFD = unixFD
        self.vsock = vsock
    }

    func start() {
        let v = vsock.fileDescriptor
        spawn { self.pump(from: self.unixFD, to: v) }
        spawn { self.pump(from: v, to: self.unixFD) }
    }

    private func spawn(_ body: @escaping () -> Void) {
        let t = Thread(block: body)
        t.stackSize = 256 << 10
        t.start()
    }

    private func pump(from: Int32, to: Int32) {
        let size = 64 << 10
        let buf = UnsafeMutableRawPointer.allocate(byteCount: size, alignment: 16)
        defer { buf.deallocate() }
        while true {
            let n = read(from, buf, size)
            if n < 0 && errno == EINTR { continue }
            if n <= 0 || !writeAll(to, buf, n) { break }
        }
        // The other side sees the end of this direction; the reverse goes on.
        _ = shutdown(to, SHUT_WR)
        _ = shutdown(from, SHUT_RD)
        lock.lock()
        open -= 1
        let last = open == 0
        lock.unlock()
        if last {
            close(unixFD)
            let v = vsock
            DispatchQueue.main.async { v.close() }
        }
    }
}
