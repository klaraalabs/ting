// swift-tools-version: 6.0
import PackageDescription

let package = Package(
    name: "Ting",
    platforms: [.iOS("16.4"), .macOS(.v12)],
    products: [.library(name: "Ting", targets: ["Ting"])],
    targets: [
        .target(name: "Ting", path: "sdks/swift/Sources/Ting"),
        .testTarget(name: "TingTests", dependencies: ["Ting"], path: "sdks/swift/Tests/TingTests"),
    ],
    swiftLanguageModes: [.v6]
)
