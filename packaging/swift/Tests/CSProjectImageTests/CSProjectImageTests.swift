import CryptoKit
import CSProjectImage
import XCTest

final class CSProjectImageTests: XCTestCase {
    func testArchiveMatchesPublishedSHA256() {
        let digest = SHA256.hash(data: CSProjectImage.archiveData).map { String(format: "%02x", $0) }.joined()
        XCTAssertEqual(digest, CSProjectImage.sha256)
        XCTAssertFalse(CSProjectImage.reference.isEmpty)
    }
}
