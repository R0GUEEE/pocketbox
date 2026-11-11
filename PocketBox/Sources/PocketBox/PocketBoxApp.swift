import SwiftUI

@main
struct PocketBoxApp: App {
    @StateObject private var model = AppModel.shared
    @Environment(\.scenePhase) private var phase

    init() {
        Layout.prepare()
        AppModel.shared.prepareRootfs()
    }

    var body: some Scene {
        WindowGroup {
            RootView()
                .environmentObject(model)
                .preferredColorScheme(nil)
                .onChange(of: phase) { newPhase in
                    if newPhase == .active, model.rootfsState == .ready {
                        model.refresh()
                    }
                }
        }
    }
}

struct RootView: View {
    @EnvironmentObject private var model: AppModel
    @State private var tab: Tab = .boxes

    enum Tab: String, CaseIterable, Identifiable {
        case boxes = "Containers"
        case images = "Images"
        case console = "Console"
        case settings = "Engine"
        var id: String { rawValue }
        var icon: String {
            switch self {
            case .boxes: return "shippingbox"
            case .images: return "square.stack.3d.up"
            case .console: return "terminal"
            case .settings: return "gauge.with.dots.needle.67percent"
            }
        }
    }

    var body: some View {
        TabView(selection: $tab) {
            ContainersView()
                .tabItem { Label(Tab.boxes.rawValue, systemImage: Tab.boxes.icon) }
                .tag(Tab.boxes)
            ImagesView()
                .tabItem { Label(Tab.images.rawValue, systemImage: Tab.images.icon) }
                .tag(Tab.images)
            ConsoleView()
                .tabItem { Label(Tab.console.rawValue, systemImage: Tab.console.icon) }
                .tag(Tab.console)
            EngineView()
                .tabItem { Label(Tab.settings.rawValue, systemImage: Tab.settings.icon) }
                .tag(Tab.settings)
        }
        .overlay(alignment: .top) {
            if case .running(let message) = model.rootfsState {
                PrepBanner(message: message)
            }
        }
        .tint(.orange)
    }
}

struct PrepBanner: View {
    let message: String
    var body: some View {
        HStack(spacing: 10) {
            ProgressView().tint(.white)
            Text("Preparing Linux guest — \(message)")
                .font(.footnote.weight(.medium))
                .foregroundStyle(.white)
            Spacer(minLength: 0)
        }
        .padding(.horizontal, 16)
        .padding(.vertical, 10)
        .background(Color.black.opacity(0.85), in: Capsule())
        .padding(.top, 4)
    }
}

/// Shared sheet that shows a command's output.
struct OutputSheet: View {
    @Environment(\.dismiss) private var dismiss
    let result: CommandResult
    @State private var copied = false

    var body: some View {
        NavigationStack {
            ScrollView {
                VStack(alignment: .leading, spacing: 12) {
                    HStack {
                        Label(result.ok ? "Success" : "Exit \(result.exitCode)",
                              systemImage: result.ok ? "checkmark.circle.fill" : "exclamationmark.triangle.fill")
                            .foregroundStyle(result.ok ? .green : .orange)
                            .font(.subheadline.weight(.semibold))
                        Spacer()
                        Text(String(format: "%.1fs", result.duration))
                            .font(.caption.monospaced())
                            .foregroundStyle(.secondary)
                    }
                    if !result.stdout.isEmpty {
                        Text(result.stdout)
                            .font(.system(.caption, design: .monospaced))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    if !result.stderr.isEmpty {
                        Text(result.stderr)
                            .font(.system(.caption, design: .monospaced))
                            .foregroundStyle(.orange)
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                    }
                    if result.stdout.isEmpty && result.stderr.isEmpty {
                        Text("No output.").foregroundStyle(.secondary)
                    }
                }
                .padding()
            }
            .navigationTitle(result.title)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) {
                    Button("Done") { dismiss() }
                }
                ToolbarItem(placement: .primaryAction) {
                    Button(copied ? "Copied" : "Copy") {
                        UIPasteboard.general.string = result.stdout + result.stderr
                        copied = true
                    }
                }
            }
        }
    }
}

struct Placeholder: View {
    let icon: String
    let title: String
    let message: String
    var body: some View {
        VStack(spacing: 10) {
            Image(systemName: icon).font(.system(size: 34)).foregroundStyle(.tertiary)
            Text(title).font(.headline)
            Text(message).font(.footnote).foregroundStyle(.secondary)
                .multilineTextAlignment(.center)
        }
        .frame(maxWidth: .infinity)
        .padding(.vertical, 40)
    }
}

extension String {
    /// Short image digest for display: sha256:abc12…
    var shortDigest: String {
        guard let i = firstIndex(of: ":") else { return self }
        let hex = self[index(after: i)...]
        return String(self[..<i]) + ":" + String(hex.prefix(12))
    }
}
