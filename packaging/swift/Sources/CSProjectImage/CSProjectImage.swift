import CCSProjectImage
import Foundation

/// The gzipped OCI archive of the project VM launcher image, compiled into the binary.
public enum CSProjectImage {
    /// The image reference in the archive.
    public static var reference: String { String(cString: cs_project_image_reference()) }

    /// Lowercase hexadecimal SHA-256 of the archive, as the build published it.
    public static var sha256: String { String(cString: cs_project_image_sha256()) }

    /// The archive bytes.
    public static var archiveData: Data {
        Data(bytes: cs_project_image_bytes(), count: Int(cs_project_image_len()))
    }
}
