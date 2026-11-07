import Foundation

public struct TingHTTPRequest: Sendable {
    public let method: String
    public let url: URL
    public let headers: [String: String]
    public let body: Data?

    public init(method: String, url: URL, headers: [String: String], body: Data? = nil) {
        self.method = method
        self.url = url
        self.headers = headers
        self.body = body
    }
}

public struct TingHTTPResponse: Sendable {
    public let status: Int
    public let headers: [String: String]
    public let body: Data

    public init(status: Int, headers: [String: String] = [:], body: Data = Data()) {
        self.status = status
        self.headers = Dictionary(headers.map { ($0.key.lowercased(), $0.value) }, uniquingKeysWith: { _, new in new })
        self.body = body
    }
}

public protocol TingHTTPTransport: Sendable {
    func send(_ request: TingHTTPRequest) async throws -> TingHTTPResponse
}

public final class URLSessionTingTransport: TingHTTPTransport {
    private let session: URLSession

    public convenience init(requestTimeout: TimeInterval = 15) {
        let configuration = URLSessionConfiguration.ephemeral
        configuration.timeoutIntervalForRequest = requestTimeout
        configuration.timeoutIntervalForResource = requestTimeout
        configuration.httpShouldSetCookies = false
        configuration.httpCookieStorage = nil
        configuration.urlCredentialStorage = nil
        configuration.urlCache = nil
        self.init(configuration: configuration)
    }

    init(configuration: URLSessionConfiguration) {
        session = URLSession(configuration: configuration, delegate: NoRedirects(), delegateQueue: nil)
    }

    public func send(_ request: TingHTTPRequest) async throws -> TingHTTPResponse {
        var urlRequest = URLRequest(url: request.url)
        urlRequest.httpMethod = request.method
        urlRequest.allHTTPHeaderFields = request.headers
        urlRequest.httpBody = request.body
        let (body, response) = try await session.data(for: urlRequest)
        guard let response = response as? HTTPURLResponse else { throw TingFailure.invalidServerResponse }
        let headers = response.allHeaderFields.reduce(into: [String: String]()) { headers, field in
            if let key = field.key as? String, let value = field.value as? String { headers[key] = value }
        }
        return TingHTTPResponse(status: response.statusCode, headers: headers, body: body)
    }

    private final class NoRedirects: NSObject, URLSessionTaskDelegate {
        func urlSession(_ session: URLSession, task: URLSessionTask, willPerformHTTPRedirection response: HTTPURLResponse,
                        newRequest request: URLRequest, completionHandler: @escaping @Sendable (URLRequest?) -> Void) {
            completionHandler(nil)
        }
    }
}

struct ServerError: Decodable {
    let error: Detail
    struct Detail: Decodable { let code: String }
}

extension TingHTTPResponse {
    var errorCode: String {
        (try? JSONDecoder().decode(ServerError.self, from: body).error.code) ?? "http_error"
    }

    var retryAfter: Date? {
        guard let value = headers["retry-after"] else { return nil }
        if let seconds = TimeInterval(value), seconds.isFinite, seconds >= 0 {
            return Date().addingTimeInterval(min(seconds, 31_536_000))
        }
        let formatter = DateFormatter()
        formatter.locale = Locale(identifier: "en_US_POSIX")
        formatter.timeZone = TimeZone(secondsFromGMT: 0)
        formatter.dateFormat = "EEE, dd MMM yyyy HH:mm:ss z"
        return formatter.date(from: value)
    }
}
