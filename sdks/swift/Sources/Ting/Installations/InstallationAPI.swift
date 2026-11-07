import Foundation

/// Wire format of the installation endpoints. Holds no state beyond configuration.
struct InstallationAPI: Sendable {
    let configuration: TingConfiguration

    func url(_ installationID: UUID) -> URL {
        configuration.endpoint.appendingPathComponent("v1/installations")
            .appendingPathComponent(installationID.uuidString.lowercased())
    }

    /// `ownership` is omitted for an installation that has never been acknowledged.
    func register(_ installationID: UUID, body: RegistrationBody, proof: String, ownership: String?) throws -> TingHTTPRequest {
        var headers = ["Authorization": "Bearer " + proof, "X-Ting-App-Id": configuration.appID,
                       "Content-Type": "application/json"]
        headers["X-Ting-Installation-Token"] = ownership
        return TingHTTPRequest(method: "PUT", url: url(installationID), headers: headers, body: try JSONEncoder().encode(body))
    }

    func revoke(_ installationID: UUID, credential: String, generation: Int64) -> TingHTTPRequest {
        TingHTTPRequest(method: "DELETE", url: url(installationID),
            headers: ["Authorization": "Bearer " + credential, "X-Ting-App-Id": configuration.appID,
                      "X-Ting-Installation-Generation": String(generation)])
    }

    /// True only when the server acknowledged exactly the prepared registration.
    func acknowledges(_ response: TingHTTPResponse, _ installationID: UUID, _ body: RegistrationBody) -> Bool {
        struct Acknowledgment: Decodable {
            let installationId: UUID
            let installationToken: String
            let generation: Int64
            let status: String
        }
        guard let acknowledgment = try? JSONDecoder().decode(Acknowledgment.self, from: response.body) else { return false }
        return acknowledgment.installationId == installationID && acknowledgment.installationToken == body.installationToken
            && acknowledgment.generation == body.generation && acknowledgment.status == "registered"
    }
}
