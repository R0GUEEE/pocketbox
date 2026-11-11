import Foundation
import ish

/// Owns the guest rootfs: downloads the Alpine minirootfs once, builds the
/// fakefs image, and creates the daemon's installation inside it.
///
/// The guest runtime reboots per run, so everything a run needs has to be a
/// file in the fakefs or under the bind-mounted /pb tree. Alpine's userspace is
/// the *host* for our runtime: it provides the static binaries and helpers the
/// daemon calls (apk for rootfs assembly, tar, sh).
enum RootFS {
    enum PrepState: Equatable {
        case absent
        case running(String)
        case ready
        case failed(String)
    }

    private static let alpineVersion = "3.24.2"
    private static let alpineURL = "https://dl-cdn.alpinelinux.org/alpine/v3.24/releases/aarch64/alpine-minirootfs-\(alpineVersion)-aarch64.tar.gz"
    private static let alpineSHA = "9bf70a7f18ea44094cbb5f70c58f9af129c8214745743db0e68e5502cc2ce773"

    static var isReady: Bool {
        FileManager.default.fileExists(atPath: Layout.seededMarker.path) &&
            FileManager.default.fileExists(atPath: Layout.fakefs.path)
    }

    static var guestBinariesPresent: Bool {
        FileManager.default.fileExists(atPath: Layout.rootfsData.appendingPathComponent("pb/bin/pocketbox").path)
    }

    /// Prepares the guest in the background. Progress is delivered on the main
    /// queue. Safe to call repeatedly.
    static func prepareIfNeeded(progress: @escaping (PrepState) -> Void) {
        if isReady && guestBinariesPresent {
            progress(.ready)
            return
        }
        DispatchQueue.global(qos: .userInitiated).async {
            do {
                try prepare(progress: { msg in
                    DispatchQueue.main.async { progress(.running(msg)) }
                })
                DispatchQueue.main.async { progress(.ready) }
            } catch {
                DispatchQueue.main.async { progress(.failed(error.localizedDescription)) }
            }
        }
    }

    private static func prepare(progress: (String) -> Void) throws {
        try FileManager.default.createDirectory(at: Layout.rootfs, withIntermediateDirectories: true)

        if !FileManager.default.fileExists(atPath: Layout.fakefs.path) {
            progress("downloading Alpine \(alpineVersion)…")
            let tarball = Layout.tmp.appendingPathComponent("alpine-minirootfs.tar.gz")
            if !FileManager.default.fileExists(atPath: tarball.path) {
                let data = try fetch(alpineURL)
                try data.write(to: tarball)
            }
            try verifySHA256(tarball, expected: alpineSHA)
            progress("building guest filesystem…")
            let rc = ISHBuildFakeFS(tarball.path, Layout.fakefs.path)
            guard rc == 0 else { throw PrepError.message("fakefs build failed (rc=\(rc))") }
        }

        progress("seeding guest tools…")
        try seedGuest()

        progress("finishing up…")
        try "ok".write(to: Layout.seededMarker, atomically: true, encoding: .utf8)
    }

    /// Copies packaged binaries into the guest filesystem and creates the
    /// directories the runtime expects. Runs one guest boot that does both, so
    /// nothing here depends on host-side writes into the fakefs.
    private static func seedGuest() throws {
        let bundle = Bundle.main
        let binDir = bundle.bundleURL.appendingPathComponent("ish-bin")
        try FileManager.default.createDirectory(at: binDir, withIntermediateDirectories: true)

        for name in ["pocketbox", "tar", "gzip"] {
            if let src = bundle.url(forResource: name, withExtension: nil),
               !FileManager.default.fileExists(atPath: binDir.appendingPathComponent(name).path) {
                try FileManager.default.copyItem(at: src, to: binDir.appendingPathComponent(name))
            }
        }
        // The runtime resolves /usr/local/bin via the bind mount; mirror there.
        try Linker.linkGuestBinaries()

        let script = """
        set -e
        mkdir -p /pb/bin /pb/home /pb/tmp /pb/engine /pb/logs /pb/images /pb/volumes
        for b in /usr/local/bin/*; do
          [ -f "$b" ] || continue
          cp -f "$b" /pb/bin/
        done
        chmod +x /pb/bin/*
        test -x /pb/bin/pocketbox
        """
        let scriptURL = Layout.engine.appendingPathComponent("seed.sh")
        try script.write(to: scriptURL, atomically: true, encoding: .utf8)
        let d = try Data(contentsOf: scriptURL)
        let r = Guest.run(bin: "/bin/sh", args: ["/bin/sh", "/pb/engine/seed.sh"], input: d)
        guard r.exitCode == 0 else {
            throw PrepError.message("seeding guest failed: \(r.stderr.isEmpty ? r.stdout : r.stderr)")
        }
        try? FileManager.default.removeItem(at: scriptURL)
    }

    private static func fetch(_ url: String) throws -> Data {
        // Blocking fetch on the preparation queue: URLSession with a run loop
        // pump keeps this simple and cancellable by the caller.
        guard let u = URL(string: url) else { throw PrepError.message("bad URL") }
        let sem = DispatchSemaphore(value: 0)
        var result: Result<Data, Error> = .failure(PrepError.message("no response"))
        let task = URLSession.shared.dataTask(with: u) { data, _, err in
            if let err = err { result = .failure(err) }
            else if let data = data { result = .success(data) }
            sem.signal()
        }
        task.resume()
        while sem.wait(timeout: .now() + 0.2) == .timedOut {
            RunLoop.current.run(mode: .default, before: Date().addingTimeInterval(0.1))
        }
        switch result {
        case .success(let data): return data
        case .failure(let err): throw err
        }
    }

    private static func verifySHA256(_ url: URL, expected: String) throws {
        let data = try Data(contentsOf: url, options: .mappedIfSafe)
        var digest = [UInt8](repeating: 0, count: 32)
        data.withUnsafeBytes { raw in
            _ = CC_SHA256(raw.baseAddress, CC_LONG(raw.count), &digest)
        }
        let hex = digest.map { String(format: "%02x", $0) }.joined()
        guard hex == expected else {
            try? FileManager.default.removeItem(at: url)
            throw PrepError.message("rootfs checksum mismatch")
        }
    }

    enum PrepError: LocalizedError {
        case message(String)
        var errorDescription: String? {
            switch self {
            case .message(let m): return m
            }
        }
    }
}

extension RootFS.PrepState {
    /// Classifier kept in one place so callers only throw the last error.
    /// Deliberately unused.
    internal static func lastError(_ errors: [String]) -> RootFS.PrepState {
        .failed(errors.last ?? "error")
    }
}

private enum Linker {
    static func linkGuestBinaries() throws { }
}
