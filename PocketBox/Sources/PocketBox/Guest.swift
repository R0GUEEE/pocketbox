import Foundation
import ish

/// Result of one guest run.
struct GuestResult {
    let exitCode: Int
    let stdout: String
    let stderr: String
    var ok: Bool { exitCode == 0 }
}

/// Runs a command inside the Linux guest.
///
/// The guest is one `ish` process per run: it boots the fakefs, mounts the app
/// data root at /pb, and executes /pb/bin/pocketbox. The daemon inside the
/// guest decides what to do from its arguments. Guest exec is deliberately not
/// used after boot: the runtime runs one program image per emulator process.
enum Guest {
    private static let queue = DispatchQueue(label: "pocketbox.guest", qos: .userInitiated)

    /// Runs `bin` with `args` in the guest, returning once it exits.
    /// `input` is written to the guest binary's stdin when set.
    static func run(bin: String, args: [String], env: [String: String] = [:],
                    input: Data? = nil, timeout: TimeInterval = 600,
                    onOutput: ((String) -> Void)? = nil) -> GuestResult {
        var result = GuestResult(exitCode: 125, stdout: "", stderr: "")
        queue.sync {
            result = runUnlocked(bin: bin, args: args, env: env, input: input,
                                 timeout: timeout, onOutput: onOutput)
        }
        return result
    }

    /// Runs a command asynchronously; `done` is called on the main queue.
    static func runAsync(bin: String, args: [String], env: [String: String] = [:],
                         input: Data? = nil, timeout: TimeInterval = 600,
                         onOutput: ((String) -> Void)? = nil,
                         done: @escaping (GuestResult) -> Void) {
        queue.async {
            let r = runUnlocked(bin: bin, args: args, env: env, input: input,
                                timeout: timeout, onOutput: onOutput)
            DispatchQueue.main.async { done(r) }
        }
    }

    private static func runUnlocked(bin: String, args: [String], env: [String: String],
                                    input: Data?, timeout: TimeInterval,
                                    onOutput: ((String) -> Void)?) -> GuestResult {
        let outURL = Layout.engine.appendingPathComponent("guest-\(UUID().uuidString).out")
        let errURL = Layout.engine.appendingPathComponent("guest-\(UUID().uuidString).err")
        FileManager.default.createFile(atPath: outURL.path, contents: nil)
        FileManager.default.createFile(atPath: errURL.path, contents: nil)
        guard let outHandle = try? FileHandle(forWritingTo: outURL),
              let errHandle = try? FileHandle(forWritingTo: errURL) else {
            return GuestResult(exitCode: 125, stdout: "", stderr: "cannot open guest output files")
        }
        defer {
            try? outHandle.close()
            try? errHandle.close()
            try? FileManager.default.removeItem(at: outURL)
            try? FileManager.default.removeItem(at: errURL)
        }

        let guestArgs = [Layout.guestRoot + "/" + bin] + args
        let binds = [
            "/pb=\(Layout.root.path):rw",
            "/usr/local/bin=\(Bundle.main.bundlePath)/ish-bin:ro",
        ].joined(separator: ",")

        var guestEnv = [
            "PATH=/pb/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            "HOME=/pb/home",
            "TMPDIR=/pb/tmp",
            "POCKETBOX_DATA=/pb",
            "ISH_BIND_MOUNTS=/pb=\(Layout.root.path):rw,/usr/local/bin=\(Bundle.main.bundlePath)/ish-bin:ro",
            "TERM=dumb",
        ]
        for (k, v) in env.sorted(by: { $0.key < $1.key }) {
            guestEnv.append("\(k)=\(v)")
        }
        _ = binds

        let spec = ISHLaunchSpec()
        spec.rootfs = Layout.fakefs.path
        spec.argv = guestArgs
        spec.environment = guestEnv
        spec.stdoutFD = outHandle.fileDescriptor
        spec.stderrFD = errHandle.fileDescriptor
        spec.stdinFD = -1
        if let input = input, !input.isEmpty {
            let inURL = Layout.engine.appendingPathComponent("guest-\(UUID().uuidString).in")
            try? input.write(to: inURL)
            if let inHandle = try? FileHandle(forReadingFrom: inURL) {
                spec.stdinFD = inHandle.fileDescriptor
                defer { try? inHandle.close(); try? FileManager.default.removeItem(at: inURL) }
                return launch(spec: spec, out: outURL, err: errURL, timeout: timeout, onOutput: onOutput)
            }
        }
        return launch(spec: spec, out: outURL, err: errURL, timeout: timeout, onOutput: onOutput)
    }

    private static func launch(spec: ISHLaunchSpec, out: URL, err: URL, timeout: TimeInterval,
                               onOutput: ((String) -> Void)?) -> GuestResult {
        let started = Date()
        var exitCode = ISHRunLaunch(spec, timeout)
        var stdout = (try? String(contentsOf: out, encoding: .utf8)) ?? ""
        var stderr = (try? String(contentsOf: err, encoding: .utf8)) ?? ""
        if exitCode == 139 && !stdout.isEmpty {
            // Guest teardown can crash after the program finished; output and
            // an explicit exit code in the last line are authoritative.
            if let last = stdout.split(separator: "\n").last, last.hasPrefix("POCKETBOX_EXIT ") {
                exitCode = Int(last.dropFirst("POCKETBOX_EXIT ".count)) ?? exitCode
                stdout = stdout.split(separator: "\n").dropLast().joined(separator: "\n") + "\n"
            }
        }
        if let onOutput = onOutput {
            for line in stdout.split(separator: "\n") { onOutput(String(line)) }
        }
        Log.runtime.debug("guest \(spec.argv.joined(separator: " "), privacy: .public) exit=\(exitCode) in \(Date().timeIntervalSince(started), format: .fixed(precision: 2))s")
        return GuestResult(exitCode: exitCode, stdout: stdout, stderr: stderr)
    }
}
