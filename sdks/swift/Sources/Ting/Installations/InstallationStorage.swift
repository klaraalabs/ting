import CryptoKit
import Foundation
import Security

/// Writes must atomically replace the complete value. A failed save must preserve the old value.
/// A locked/inaccessible store must throw; only a genuinely absent value returns nil.
public protocol InstallationStorage: Sendable {
    func load(namespace: String) throws -> Data?
    func save(_ data: Data, namespace: String) throws
}

public final class KeychainInstallationStorage: InstallationStorage {
    private let service: String

    public init(service: String = "com.klaraalabs.ting.installations") {
        self.service = service
    }

    private func query(_ namespace: String) -> [String: Any] {
        let account = SHA256.hash(data: Data(namespace.utf8)).map { String(format: "%02x", $0) }.joined()
        return [kSecClass as String: kSecClassGenericPassword, kSecAttrService as String: service,
                kSecAttrAccount as String: account, kSecAttrSynchronizable as String: false]
    }

    public func load(namespace: String) throws -> Data? {
        var query = query(namespace)
        query[kSecReturnData as String] = true
        query[kSecMatchLimit as String] = kSecMatchLimitOne
        var result: CFTypeRef?
        let status = SecItemCopyMatching(query as CFDictionary, &result)
        if status == errSecItemNotFound { return nil }
        guard status == errSecSuccess, let data = result as? Data else { throw TingFailure.storageUnavailable }
        return data
    }

    /// Readable after the first unlock since boot, so prewarmed and background launches can construct a client.
    /// Updating an existing record also migrates its accessibility class.
    public func save(_ data: Data, namespace: String) throws {
        let query = query(namespace)
        let attributes = [kSecValueData as String: data,
                          kSecAttrAccessible as String: kSecAttrAccessibleAfterFirstUnlockThisDeviceOnly] as [String: Any]
        let status = SecItemUpdate(query as CFDictionary, attributes as CFDictionary)
        if status == errSecItemNotFound {
            let add = query.merging(attributes) { _, new in new }
            guard SecItemAdd(add as CFDictionary, nil) == errSecSuccess else { throw TingFailure.storageUnavailable }
        } else if status != errSecSuccess {
            throw TingFailure.storageUnavailable
        }
    }
}

func installationCredential() throws -> String {
    var bytes = [UInt8](repeating: 0, count: 32)
    guard SecRandomCopyBytes(kSecRandomDefault, bytes.count, &bytes) == errSecSuccess else {
        throw TingFailure.randomnessUnavailable
    }
    return "ting_ik_" + Data(bytes).base64EncodedString()
        .replacingOccurrences(of: "+", with: "-").replacingOccurrences(of: "/", with: "_")
        .replacingOccurrences(of: "=", with: "")
}

func validInstallationCredential(_ value: String) -> Bool {
    guard value.hasPrefix("ting_ik_"), value.utf8.count == 51 else { return false }
    let raw = String(value.dropFirst(8))
    let base64 = raw.replacingOccurrences(of: "-", with: "+").replacingOccurrences(of: "_", with: "/")
    guard let data = Data(base64Encoded: base64 + "="), data.count == 32 else { return false }
    return data.base64EncodedString().replacingOccurrences(of: "+", with: "-")
        .replacingOccurrences(of: "/", with: "_").replacingOccurrences(of: "=", with: "") == raw
}

final class InstallationOwnership: @unchecked Sendable {
    static let shared = InstallationOwnership()
    private let lock = NSLock()
    private var namespaces = Set<String>()

    func acquire(_ namespace: String) throws {
        lock.lock()
        defer { lock.unlock() }
        guard namespaces.insert(namespace).inserted else { throw TingFailure.clientAlreadyExists }
    }

    func release(_ namespace: String) {
        lock.lock()
        defer { lock.unlock() }
        namespaces.remove(namespace)
    }
}
