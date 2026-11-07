import Foundation
import XCTest
@testable import Ting

private final class StubURLProtocol: URLProtocol, @unchecked Sendable {
    override class func canInit(with request: URLRequest) -> Bool { true }
    override class func canonicalRequest(for request: URLRequest) -> URLRequest { request }
    override func startLoading() {
        guard request.httpMethod == "DELETE",
              request.value(forHTTPHeaderField: "X-Ting-Installation-Generation") == "42",
              request.value(forHTTPHeaderField: "X-Ting-App-Id") == "app_test",
              request.value(forHTTPHeaderField: "Authorization") == "Bearer fixture"
        else { client?.urlProtocol(self, didFailWithError: URLError(.badURL)); return }
        let response = HTTPURLResponse(url: request.url!, statusCode: 429, httpVersion: "HTTP/1.1", headerFields: ["Retry-After": "120"])!
        client?.urlProtocol(self, didReceive: response, cacheStoragePolicy: .notAllowed)
        client?.urlProtocol(self, didLoad: Data("{\"error\":{\"code\":\"rate_limited\"}}".utf8))
        client?.urlProtocolDidFinishLoading(self)
    }
    override func stopLoading() {}
}

@MainActor
final class HTTPTransportTests: XCTestCase {
    func testURLSessionPreservesWireHeadersAndRetryAfter() async throws {
        let config = URLSessionConfiguration.ephemeral
        config.protocolClasses = [StubURLProtocol.self]
        let transport = URLSessionTingTransport(configuration: config)
        let response = try await transport.send(TingHTTPRequest(method: "DELETE", url: URL(string: "https://ting.test/v1/installations/id")!,
            headers: ["Authorization": "Bearer fixture", "X-Ting-App-Id": "app_test", "X-Ting-Installation-Generation": "42"]))
        XCTAssertEqual(response.status, 429)
        XCTAssertEqual(response.errorCode, "rate_limited")
        XCTAssertGreaterThan(response.retryAfter!.timeIntervalSinceNow, 119)
    }
}
