// swift-tools-version: 6.0
import PackageDescription

// build.sh copies the gzipped OCI archive of the launcher image to
// Sources/CCSProjectImage/cs-project-image.tar.gz and generates embedded_image.c, which includes
// it with `#embed`. socktainer carries the image in its binary, with no resource bundle.
let package = Package(
    name: "CSProjectImage",
    platforms: [.macOS(.v13)],
    products: [.library(name: "CSProjectImage", targets: ["CSProjectImage"])],
    targets: [
        .target(name: "CCSProjectImage", exclude: ["cs-project-image.tar.gz"]),
        .target(name: "CSProjectImage", dependencies: ["CCSProjectImage"]),
        .testTarget(name: "CSProjectImageTests", dependencies: ["CSProjectImage"]),
    ]
)
