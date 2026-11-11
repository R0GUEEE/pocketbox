import Foundation
import SwiftUI
import Combine

/// One engine command executed in the guest, with its captured output.
struct CommandResult: Identifiable, Equatable {
    let id = UUID()
    let title: String
    let exitCode: Int
    let stdout: String
    let stderr: String
    let duration: TimeInterval
    let started: Date

    var ok: Bool { exitCode == 0 }
}

/// A parsed line of `docker ps` style output produced by the engine's ps verb.
struct ContainerRow: Identifiable, Equatable {
    var id: String { shortID }
    let shortID: String
    let image: String
    let command: String
    let created: String
    let status: String
    let ports: String
    let names: String
    let running: Bool
}

struct ImageRow: Identifiable, Equatable {
    var id: String { digest }
    let reference: String
    let digest: String
    let size: Int64
    let created: String
}

final class AppModel: ObservableObject {
    static let shared = AppModel()

    @Published var rootfsState: RootFS.PrepState = RootFS.isReady ? .ready : .absent
    @Published var engineState: String = "stopped"
    @Published var containers: [ContainerRow] = []
    @Published var images: [ImageRow] = []
    @Published var history: [CommandResult] = []
    @Published var busy: Bool = false
    @Published var endpoint: String = "tcp://127.0.0.1:2375"
    @Published var lastError: String?
    @Published var engineVersion: String?
    @Published var diskUsage: String?

    private let parseQueue = DispatchQueue(label: "pocketbox.parse")

    func prepareRootfs() {
        guard rootfsState != .ready else { return }
        RootFS.prepareIfNeeded { [weak self] state in
            guard let self = self else { return }
            self.rootfsState = state
            if state == .ready { self.refresh() }
        }
    }

    /// Runs one engine command in the guest and refreshes cached state.
    func run(_ title: String, _ bin: String, _ args: [String], input: Data? = nil,
             refresh: Bool = true, completion: ((CommandResult) -> Void)? = nil) {
        guard !busy else { return }
        if rootfsState != .ready {
            prepareRootfs()
            lastError = "guest is still preparing"
            return
        }
        busy = true
        lastError = nil
        let started = Date()
        Guest.runAsync(bin: bin, args: args, input: input) { [weak self] result in
            guard let self = self else { return }
            let cmd = CommandResult(title: title, exitCode: result.exitCode, stdout: result.stdout,
                                    stderr: result.stderr, duration: Date().timeIntervalSince(started),
                                    started: started)
            self.busy = false
            self.history.insert(cmd, at: 0)
            if self.history.count > 50 { self.history.removeLast(self.history.count - 50) }
            if !cmd.ok {
                self.lastError = cmd.stderr.isEmpty ? cmd.stdout : cmd.stderr
            }
            EngineLog.shared.append("$ \(title) → exit \(cmd.exitCode) in \(String(format: "%.1f", cmd.duration))s")
            completion?(cmd)
            if refresh && cmd.ok { self.refresh() }
        }
    }

    func refresh() {
        guard rootfsState == .ready, !busy else { return }

        refreshContainers()
        refreshImages()
    }

    func refreshContainers() {
        Guest.runAsync(bin: "/pb/bin/pocketbox", args: ["ps", "-a"]) { [weak self] r in
            guard let self = self else { return }
            self.parseQueue.async {
                let rows = Self.parsePS(r.stdout)
                DispatchQueue.main.async { self.containers = rows }
            }
        }
        Guest.runAsync(bin: "/pb/bin/pocketbox", args: ["version"]) { [weak self] r in
            DispatchQueue.main.async {
                if r.exitCode == 0 {
                    self?.engineVersion = r.stdout.trimmingCharacters(in: .whitespacesAndNewlines)
                }
            }
        }
    }

    func refreshImages() {
        Guest.runAsync(bin: "/pb/bin/pocketbox", args: ["images"]) { [weak self] r in
            guard let self = self else { return }
            let rows = Self.parseImages(r.stdout)
            DispatchQueue.main.async { self.images = rows }
        }
    }

    func startEngine() {
        guard !busy else { return }
        engineState = "starting"
        run("start daemon", "/pb/bin/pocketbox", ["serve", "--listen", "tcp://0.0.0.0:2375"]) { [weak self] r in
            self?.engineState = r.ok ? "last run ok" : "stopped"
        }
    }

    func pull(_ reference: String) {
        let ref = reference.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !ref.isEmpty else { return }
        run("pull \(ref)", "/pb/bin/pocketbox", ["pull", ref])
    }

    func start(_ row: ContainerRow) {
        run("start \(row.names)", "/pb/bin/pocketbox", ["start", row.names])
    }

    func stop(_ row: ContainerRow) {
        run("stop \(row.names)", "/pb/bin/pocketbox", ["stop", row.names])
    }

    func remove(_ row: ContainerRow) {
        run("rm \(row.names)", "/pb/bin/pocketbox", ["rm", row.names])
    }

    func logs(_ row: ContainerRow) -> Data {
        let r = Guest.run(bin: "/pb/bin/pocketbox", args: ["logs", "--tail", "200", row.names])
        return Data((r.stdout + r.stderr).utf8)
    }

    func removeImage(_ row: ImageRow) {
        run("rmi \(row.reference)", "/pb/bin/pocketbox", ["rmi", row.reference])
    }

    func runCommand(_ command: String) {
        let parts = CommandLineParser.split(command)
        guard !parts.isEmpty else { return }
        run(command, "/pb/bin/pocketbox", parts)
    }

    static func parsePS(_ out: String) -> [ContainerRow] {
        var rows: [ContainerRow] = []
        for line in out.split(separator: "\n") {
            let fields = line.split(separator: "\t", omittingEmptySubsequences: false).map(String.init)
            guard fields.count >= 7 else { continue }
            rows.append(ContainerRow(shortID: fields[0], image: fields[1], command: fields[2],
                                     created: fields[3], status: fields[4], ports: fields[5],
                                     names: fields[6], running: fields[4].hasPrefix("Up")))
        }
        return rows
    }

    static func parseImages(_ out: String) -> [ImageRow] {
        var rows: [ImageRow] = []
        for line in out.split(separator: "\n") {
            let fields = line.split(separator: "\t", omittingEmptySubsequences: false).map(String.init)
            guard fields.count >= 4 else { continue }
            rows.append(ImageRow(reference: fields[0], digest: fields[1],
                                 size: Int64(fields[2]) ?? 0, created: fields[3]))
        }
        return rows
    }
}

/// Minimal shell-like splitter: quotes and backslashes, nothing else.
enum CommandLineParser {
    static func split(_ s: String) -> [String] {
        var out: [String] = []
        var cur = ""
        var inSingle = false
        var inDouble = false
        var escape = false
        for ch in s {
            if escape {
                cur.append(ch)
                escape = false
                continue
            }
            switch ch {
            case "\\" where !inSingle:
                escape = true
            case "\"" where !inSingle:
                inDouble.toggle()
            case "'" where !inDouble:
                inSingle.toggle()
            case " ", "\t", "\n" where !inSingle && !inDouble:
                if !cur.isEmpty { out.append(cur); cur = "" }
            default:
                cur.append(ch)
            }
        }
        if !cur.isEmpty { out.append(cur) }
        return out
    }
}
