import Foundation
import os

/// Output/error ring handed to a `PBHostCall`.
final class HostIO {
    private let lock = NSLock()
    private var outData = Data()
    private var errData = Data()
    let outPath: String
    let inPath: String?

    init(outPath: String, inPath: String?) {
        self.outPath = outPath
        self.inPath = inPath
    }

    func writeStdout(_ text: String) {
        lock.lock(); outData.append(Data(text.utf8)); lock.unlock()
        flush()
    }

    func writeStderr(_ text: String) {
        lock.lock(); errData.append(Data(text.utf8)); lock.unlock()
        flush()
    }

    func stdoutText() -> String {
        lock.lock(); defer { lock.unlock() }
        return String(decoding: outData, as: UTF8.self)
    }

    func stderrText() -> String {
        lock.lock(); defer { lock.unlock() }
        return String(decoding: errData, as: UTF8.self)
    }

    func peekStderr() -> String { stderrText() }

    func clear() {
        lock.lock(); outData.removeAll(keepingCapacity: true); errData.removeAll(keepingCapacity: true); lock.unlock()
    }

    /// Reads stdin content for the call, if the guest supplied one.
    func readInput(into buf: UnsafeMutablePointer<Int8>, capacity: Int) -> Int32 {
        guard capacity > 0 else { return 0 }
        lock.lock()
        defer { lock.unlock() }
        guard let inPath = inPath, let data = FileManager.default.contents(atPath: inPath), !data.isEmpty else {
            return 0
        }
        let n = min(capacity, data.count)
        data.withUnsafeBytes { raw in
            if let base = raw.baseAddress {
                buf.withMemoryRebound(to: UInt8.self, capacity: n) { dst in
                    memcpy(dst, base, n)
                }
            }
        }
        try? FileManager.default.removeItem(atPath: inPath)
        return Int32(n)
    }

    private func flush() {
        let snapshot: Data
        lock.lock(); snapshot = outData; lock.unlock()
        try? snapshot.write(to: URL(fileURLWithPath: outPath))
    }
}

/// The host side of a guest entry point. The guest calls a C symbol named
/// `PBEmit_<name>_v` with a va_list built from a callback reference packed in a
/// single 64-bit cookie; see `module.modulemap` for the declaration contract.
enum HostHandlerTable {
    /// Makes a call object for one daemon invocation. `verb` identifies the
    /// daemon command (cli, ping, version, status, ...).
    static func makeCall(verb: String) -> HostCall {
        let out = Layout.engine.appendingPathComponent(String(format: "call-%d-%@.out", Int32.random(in: 0..<Int32.max), verb))
        let inURL = Layout.engine.appendingPathComponent(String(format: "call-%d-%@.in", Int32.random(in: 0..<Int32.max), verb))
        let inPath = FileManager.default.fileExists(atPath: inURL.path) ? inURL.path : nil
        return HostCalls.add(verb: verb, io: HostIO(outPath: out.path, inPath: inPath))
    }
}

/// One guest command invocation. `handle` is what the guest sees as its real
/// host fd; it hands back this object through `PBHostCallLookup`.
final class HostCall {
    let verb: String
    let io: HostIO
    private let handle: Int32

    init(verb: String, io: HostIO, handle: Int32) {
        self.verb = verb
        self.io = io
        self.handle = handle
    }
}

/// Calls into runtime entry points. Every function is `@_cdecl` so the Go
/// daemon (buildmode=c-archive) and the C host glue can reference it by symbol.
@_cdecl("pb_host_stdout_write")
public func pbHostStdoutWrite(_ handle: Int32, _ buf: UnsafePointer<Int8>?, _ len: Int32) -> Int32 {
    guard let buf = buf, len >= 0, let call = HostCalls.lookup(handle) else { return -1 }
    call.io.writeStdout(String(decoding: UnsafeBufferPointer(start: UnsafeRawPointer(buf).assumingMemoryBound(to: UInt8.self), count: Int(len)), as: UTF8.self))
    return len
}

@_cdecl("pb_host_stderr_write")
public func pbHostStderrWrite(_ handle: Int32, _ buf: UnsafePointer<Int8>?, _ len: Int32) -> Int32 {
    guard let buf = buf, len >= 0, let call = HostCalls.lookup(handle) else { return -1 }
    call.io.writeStderr(String(decoding: UnsafeBufferPointer(start: UnsafeRawPointer(buf).assumingMemoryBound(to: UInt8.self), count: Int(len)), as: UTF8.self))
    return len
}

@_cdecl("pb_host_stdin_read")
public func pbHostStdinRead(_ handle: Int32, _ buf: UnsafeMutablePointer<Int8>?, _ len: Int32) -> Int32 {
    guard let buf = buf, len >= 0, let call = HostCalls.lookup(handle) else { return 0 }
    return call.io.readInput(into: buf, capacity: Int(len))
}

@_cdecl("pb_host_stdout_close")
public func pbHostStdoutClose(_ handle: Int32) -> Int32 {
    HostCalls.remove(handle)
    return 0
}

@_cdecl("pb_host_stderr_close")
public func pbHostStderrClose(_ handle: Int32) -> Int32 { 0 }

@_cdecl("pb_host_stdin_close")
public func pbHostStdinClose(_ handle: Int32) -> Int32 { 0 }

/// Called by the Go daemon once to learn the environment it runs in.
@_cdecl("pb_host_root")
public func pbHostRoot() -> UnsafePointer<CChar> {
    HostCalls.rootCString
}

enum HostCalls {
    private static let lock = NSLock()
    private static var table: [Int32: HostCall] = [:]
    private static var next: Int32 = 0x7000_0000
    static let rootCString: UnsafePointer<CChar> = strdup(Layout.root.path)!

    static func add(verb: String, io: HostIO) -> HostCall {
        lock.lock(); defer { lock.unlock() }
        next += 1
        let call = HostCall(verb: verb, io: io, handle: next)
        table[next] = call
        return call
    }

    static func lookup(_ handle: Int32) -> HostCall? {
        lock.lock(); defer { lock.unlock() }
        return table[handle]
    }

    @discardableResult
    static func remove(_ handle: Int32) -> HostCall? {
        lock.lock(); defer { lock.unlock() }
        return table.removeValue(forKey: handle)
    }
}

