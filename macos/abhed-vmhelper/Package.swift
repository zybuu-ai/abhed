// swift-tools-version:5.9
import PackageDescription

// abhed-vmhelper runs one microVM for the microvm driver on macOS. Apple's
// frameworks only: no package dependencies.
let package = Package(
    name: "abhed-vmhelper",
    platforms: [.macOS(.v13)],
    targets: [
        .executableTarget(
            name: "abhed-vmhelper",
            path: "Sources/abhed-vmhelper",
            linkerSettings: [.linkedFramework("Virtualization"), .linkedFramework("Security")]
        )
    ]
)
