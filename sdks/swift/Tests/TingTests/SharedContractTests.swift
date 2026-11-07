import Foundation
import XCTest
@testable import Ting

@MainActor
final class SharedContractTests: XCTestCase {
    struct Contract: Decodable {
        let version: Int
        let scenarios: [Scenario]
        struct Scenario: Decodable {
            let name: String
            let steps: [Step]
            let finalUser: String
            let finalCredential: String
            let finalGeneration: Int64
            let finalActive: Bool
        }
        struct Step: Decodable {
            let method: String
            let user: String?
            let generation: Int64
            let credential: String?
            let proposed: String?
            let push: String?
            let status: Int
            let error: String?
        }
    }

    func testProtocolDoubleConformsToSharedGoFixture() async throws {
        // Keep one canonical fixture at repository level, not a drifting copy inside this package.
        let root = URL(fileURLWithPath: #filePath).deletingLastPathComponent()
            .deletingLastPathComponent().deletingLastPathComponent().deletingLastPathComponent()
            .deletingLastPathComponent()
        let fixture = root.appendingPathComponent("tests/contracts/installation-lifecycle.json")
        let contract = try JSONDecoder().decode(Contract.self, from: Data(contentsOf: fixture))
        XCTAssertEqual(contract.version, 1)
        XCTAssertFalse(contract.scenarios.isEmpty)
        for scenario in contract.scenarios {
            let server = ProtocolServer()
            let id = UUID()
            let credentials = ["": "", "first": try installationCredential(), "second": try installationCredential(), "third": try installationCredential()]
            for step in scenario.steps {
                let body = RegistrationBody(platform: "ios", pushToken: String(repeating: step.push ?? "a", count: 64),
                    apnsEnvironment: .production, installationToken: credentials[step.proposed ?? ""]!, generation: step.generation)
                let request = TingHTTPRequest(method: step.method,
                    url: URL(string: "https://ting.test/v1/installations/\(id.uuidString)")!,
                    headers: ["Authorization": "Bearer " + (step.method == "PUT" ? step.user! : credentials[step.credential ?? ""]!),
                        "X-Ting-App-Id": "app_fixture", "X-Ting-Installation-Token": credentials[step.credential ?? ""]!,
                        "X-Ting-Installation-Generation": String(step.generation)],
                    body: step.method == "PUT" ? try JSONEncoder().encode(body) : nil)
                let response = try await server.send(request)
                XCTAssertEqual(response.status, step.status, scenario.name)
                if let error = step.error { XCTAssertEqual(response.errorCode, error, scenario.name) }
            }
            let record = await server.records.values.first
            XCTAssertEqual(record?.user, scenario.finalUser, scenario.name)
            XCTAssertEqual(record?.body.installationToken, credentials[scenario.finalCredential], scenario.name)
            XCTAssertEqual(record?.generation, scenario.finalGeneration, scenario.name)
            XCTAssertEqual(record?.active, scenario.finalActive, scenario.name)
        }
    }
}
