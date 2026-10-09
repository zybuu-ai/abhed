// abhed-vmhelper runs one Linux microVM on Virtualization.framework for the
// microvm driver, speaking helper protocol 1 (driver/microvm/helper.go).
import Foundation
import Security
import Virtualization

let entitlement = "com.apple.security.virtualization"

func fail(_ message: String, _ status: Int32) -> Never {
    FileHandle.standardError.write(Data("abhed-vmhelper: \(message)\n".utf8))
    exit(status)
}

func hasEntitlement(_ name: String) -> Bool {
    guard let task = SecTaskCreateFromSelf(nil),
          let v = SecTaskCopyValueForEntitlement(task, name as CFString, nil) else { return false }
    return (v as? Bool) == true
}

func hostDescription() -> String {
    let v = ProcessInfo.processInfo.operatingSystemVersion
    var u = utsname()
    uname(&u)
    let arch = withUnsafeBytes(of: &u.machine) { String(decoding: $0.prefix(while: { $0 != 0 }), as: UTF8.self) }
    return "macOS \(v.majorVersion).\(v.minorVersion).\(v.patchVersion) on \(arch)"
}

/// check prints the protocol's one-line answer, with every reason the helper
/// cannot run a VM here, not only the first.
func check() -> Never {
    var reasons: [String] = []
    if !VZVirtualMachine.isSupported {
        reasons.append("Virtualization.framework cannot run a VM here (\(hostDescription())): it needs Apple silicon " +
                       "or an Intel Mac with VT-x, and does not run inside a VM that lacks nested virtualization")
    }
    if !hasEntitlement(entitlement) {
        reasons.append("this helper is not code-signed with the \(entitlement) entitlement")
    }
    let answer: [String: Any] = ["protocol": VMConfig.protocolVersion, "supported": reasons.isEmpty,
                                 "reason": reasons.joined(separator: "; ")]
    let data = try! JSONSerialization.data(withJSONObject: answer, options: [.sortedKeys, .withoutEscapingSlashes])
    FileHandle.standardOutput.write(data + Data("\n".utf8))
    exit(0)
}

func makeConfiguration(_ cfg: VMConfig) throws -> VZVirtualMachineConfiguration {
    let minCPU = VZVirtualMachineConfiguration.minimumAllowedCPUCount
    let maxCPU = min(VZVirtualMachineConfiguration.maximumAllowedCPUCount, ProcessInfo.processInfo.activeProcessorCount)
    guard cfg.cpus >= minCPU, cfg.cpus <= maxCPU else {
        throw HelperError("cpus \(cfg.cpus) is outside what this Mac allows (\(minCPU) to \(maxCPU))")
    }
    let minMiB = Int(VZVirtualMachineConfiguration.minimumAllowedMemorySize >> 20)
    let maxMiB = Int(min(VZVirtualMachineConfiguration.maximumAllowedMemorySize, ProcessInfo.processInfo.physicalMemory / 2) >> 20)
    guard cfg.memoryMiB >= minMiB, cfg.memoryMiB <= maxMiB else {
        throw HelperError("memory_mib \(cfg.memoryMiB) is outside what this Mac allows (\(minMiB) to \(maxMiB))")
    }

    let c = VZVirtualMachineConfiguration()
    c.platform = VZGenericPlatformConfiguration()
    c.cpuCount = cfg.cpus
    c.memorySize = UInt64(cfg.memoryMiB) << 20

    let boot = VZLinuxBootLoader(kernelURL: URL(fileURLWithPath: cfg.kernel))
    boot.commandLine = cfg.bootArgs
    c.bootLoader = boot

    c.storageDevices = try cfg.disks.map { d in
        do {
            let a = try VZDiskImageStorageDeviceAttachment(url: URL(fileURLWithPath: d.path), readOnly: d.readOnly)
            return VZVirtioBlockDeviceConfiguration(attachment: a)
        } catch {
            throw HelperError("disk \(d.path): \(error.localizedDescription)")
        }
    }
    c.socketDevices = [VZVirtioSocketDeviceConfiguration()]
    c.entropyDevices = [VZVirtioEntropyDeviceConfiguration()]
    // The guest's console goes to standard output and nothing comes back:
    // standard input is the driver's lifeline, never the guest's.
    let console = VZVirtioConsoleDeviceSerialPortConfiguration()
    console.attachment = VZFileHandleSerialPortAttachment(fileHandleForReading: nil,
                                                          fileHandleForWriting: FileHandle.standardOutput)
    c.serialPorts = [console]
    // Stated, so a default that changes cannot add a device.
    c.networkDevices = []
    c.directorySharingDevices = []
    c.audioDevices = []
    c.graphicsDevices = []
    c.keyboards = []
    c.pointingDevices = []
    c.memoryBalloonDevices = []
    if #available(macOS 14.0, *) {
        c.consoleDevices = []
    }
    if #available(macOS 15.0, *) {
        c.usbControllers = []
    }
    do {
        try c.validate()
    } catch {
        throw HelperError("the VM configuration is refused: \(error.localizedDescription)", status: ExitStatus.failed)
    }
    return c
}

/// Runner owns the VM. Everything that touches it runs on the main queue,
/// the queue the VM was made on.
final class Runner: NSObject, VZVirtualMachineDelegate, VZVirtioSocketListenerDelegate {
    let cfg: VMConfig
    let vm: VZVirtualMachine
    let device: VZVirtioSocketDevice
    let guestSockets: [UInt32: String]
    var listenFD: Int32 = -1
    var ending = false

    init(cfg: VMConfig, configuration: VZVirtualMachineConfiguration) {
        self.cfg = cfg
        vm = VZVirtualMachine(configuration: configuration, queue: .main)
        device = vm.socketDevices.first as! VZVirtioSocketDevice
        guestSockets = Dictionary(uniqueKeysWithValues: cfg.guestPorts.map { ($0.port, $0.socket) })
        super.init()
        vm.delegate = self
    }

    func start() throws {
        listenFD = try listenUnix(cfg.vsockSocket)
        // Only the ports listed get a listener: the guest's connection to
        // any other port is refused by the framework.
        for p in cfg.guestPorts {
            let l = VZVirtioSocketListener()
            l.delegate = self
            device.setSocketListener(l, forPort: p.port)
        }
        watchStdin()
        for sig in [SIGTERM, SIGINT, SIGHUP] {
            signal(sig, SIG_IGN)
            let src = DispatchSource.makeSignalSource(signal: sig, queue: .main)
            src.setEventHandler { [weak self] in self?.end(ExitStatus.stopped, "signal \(sig)") }
            src.resume()
            signals.append(src)
        }
        vm.start { [weak self] result in
            guard let self else { return }
            switch result {
            case .success:
                self.acceptLoop()
            case .failure(let e):
                self.end(ExitStatus.failed, "starting the VM: \(e.localizedDescription)")
            }
        }
    }

    private var signals: [DispatchSourceSignal] = []

    /// The driver holds the only write end of standard input: when it ends,
    /// the driver is gone, and so is the VM.
    private func watchStdin() {
        let t = Thread { [weak self] in
            var b = [UInt8](repeating: 0, count: 512)
            while true {
                let n = read(0, &b, b.count)
                if n > 0 || (n < 0 && errno == EINTR) { continue }
                break
            }
            DispatchQueue.main.async { self?.end(ExitStatus.stopped, nil) }
        }
        t.start()
    }

    private func acceptLoop() {
        let fd = listenFD
        let t = Thread { [weak self] in
            while true {
                let c = accept(fd, nil, nil)
                if c < 0 {
                    if errno == EINTR || errno == ECONNABORTED { continue }
                    return
                }
                _ = fcntl(c, F_SETFD, FD_CLOEXEC)
                var one: Int32 = 1
                _ = setsockopt(c, SOL_SOCKET, SO_NOSIGPIPE, &one, socklen_t(MemoryLayout<Int32>.size))
                Thread { self?.serveHost(c) }.start()
            }
        }
        t.start()
    }

    /// serveHost answers the driver's "CONNECT <port>" with a connection to
    /// that guest port, then joins the two.
    private func serveHost(_ fd: Int32) {
        guard let port = readHandshake(fd) else {
            close(fd)
            return
        }
        DispatchQueue.main.async {
            guard !self.ending, self.vm.state == .running else {
                close(fd)
                return
            }
            self.device.connect(toPort: port) { result in
                switch result {
                case .success(let conn):
                    guard writeString(fd, "OK \(port)\n") else {
                        close(fd)
                        conn.close()
                        return
                    }
                    Bridge(unixFD: fd, vsock: conn).start()
                case .failure:
                    // The guest is not listening yet; the driver tries again.
                    close(fd)
                }
            }
        }
    }

    func listener(_ listener: VZVirtioSocketListener, shouldAcceptNewConnection connection: VZVirtioSocketConnection,
                  from socketDevice: VZVirtioSocketDevice) -> Bool {
        guard !ending, let path = guestSockets[connection.destinationPort] else { return false }
        let fd = connectUnix(path)
        guard fd >= 0 else { return false }
        Bridge(unixFD: fd, vsock: connection).start()
        return true
    }

    func guestDidStop(_ virtualMachine: VZVirtualMachine) {
        finish(ExitStatus.stopped, nil)
    }

    func virtualMachine(_ virtualMachine: VZVirtualMachine, didStopWithError error: Error) {
        finish(ExitStatus.failed, "the VM stopped: \(error.localizedDescription)")
    }

    /// end stops the VM if it runs, then exits.
    func end(_ status: Int32, _ why: String?) {
        if ending { return }
        ending = true
        if vm.canStop {
            // A VM that will not stop does not keep the helper alive.
            DispatchQueue.main.asyncAfter(deadline: .now() + 5) { self.finish(status, why) }
            vm.stop { _ in self.finish(status, why) }
        } else {
            finish(status, why)
        }
    }

    private func finish(_ status: Int32, _ why: String?) {
        if listenFD >= 0 {
            unlink(cfg.vsockSocket)
        }
        if let why {
            FileHandle.standardError.write(Data("abhed-vmhelper: \(why)\n".utf8))
        }
        exit(status)
    }
}

signal(SIGPIPE, SIG_IGN)
let args = Array(CommandLine.arguments.dropFirst())
switch args.count {
case 1 where args[0] == "check":
    check()
case 3 where args[0] == "run" && args[1] == "--config":
    var runner: Runner?
    do {
        let cfg = try VMConfig.load(path: args[2])
        let c = try makeConfiguration(cfg)
        runner = Runner(cfg: cfg, configuration: c)
        try runner!.start()
    } catch let e as HelperError {
        fail(e.description, e.status)
    } catch {
        fail(error.localizedDescription, ExitStatus.failed)
    }
    withExtendedLifetime(runner) { dispatchMain() }
default:
    fail("usage: abhed-vmhelper check | abhed-vmhelper run --config <file>", ExitStatus.refused)
}
