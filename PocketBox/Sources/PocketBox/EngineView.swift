import SwiftUI

struct EngineView: View {
    @EnvironmentObject private var model: AppModel
    @State private var daemonPreview: String?
    @State private var showReset = false

    var body: some View {
        NavigationStack {
            List {
                Section("Guest") {
                    LabeledContent("Status") {
                        switch model.rootfsState {
                        case .ready: Label("Ready", systemImage: "checkmark.circle.fill").foregroundStyle(.green)
                        case .absent: Text("Not prepared").foregroundStyle(.secondary)
                        case .running(let m): Text(m).foregroundStyle(.secondary)
                        case .failed(let m): Text(m).foregroundStyle(.red).lineLimit(2)
                        }
                    }
                    LabeledContent("Kernel", value: "Linux \(architecture) (ish guest)")
                    if let version = model.engineVersion {
                        LabeledContent("Engine", value: version)
                    }
                    if let usage = model.diskUsage {
                        LabeledContent("Storage", value: usage)
                    }
                    Button("Re-prepare guest") {
                        if case .ready = model.rootfsState { model.rootfsState = .absent }
                        model.prepareRootfs()
                    }
                    .disabled({ if case .running = model.rootfsState { return true }; return false }())
                }

                Section("Docker API") {
                    LabeledContent("Endpoint", value: model.endpoint)
                    Text("Phase 2 of the roadmap makes this a real Docker-compatible socket. Today the API verbs run through the console and the CLI, so `docker ps` over SSH works with the bundled client.")
                        .font(.footnote)
                        .foregroundStyle(.secondary)
                    Button("Show connection snippet") {
                        daemonPreview = """
                        # from a computer on the same network, once the daemon is running
                        export DOCKER_HOST=tcp://<phone-ip>:2375
                        docker version
                        """
                    }
                }

                Section("Runtime model") {
                    Text("""
                    PocketBox runs a Linux guest per command. The guest boots in about a second, mounts this app's data directory at /pb, and executes the engine binary there. Containers share the guest's kernel, so they see one filesystem and one network, which is what a phone can honestly provide without a jailbreak.
                    """)
                    .font(.footnote)
                    .foregroundStyle(.secondary)
                }

                Section {
                    Button("Delete guest filesystem", role: .destructive) { showReset = true }
                } footer: {
                    Text("Removes the downloaded rootfs and every image layer. Containers and volumes survive in the app data directory.")
                }
            }
            .navigationTitle("Engine")
            .sheet(item: Binding(get: { daemonPreview.map(Message.init) }, set: { daemonPreview = $0?.text })) { msg in
                NavigationStack {
                    ScrollView {
                        Text(msg.text).font(.system(.caption, design: .monospaced))
                            .textSelection(.enabled)
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .padding()
                    }
                    .navigationTitle("Docker client")
                    .navigationBarTitleDisplayMode(.inline)
                    .toolbar { ToolbarItem(placement: .confirmationAction) { Button("Done") { daemonPreview = nil } } }
                }
            }
            .confirmationDialog("Delete the guest filesystem?", isPresented: $showReset, titleVisibility: .visible) {
                Button("Delete", role: .destructive) {
                    model.rootfsState = .absent
                }
            }
        }
    }

    private var architecture: String {
        #if arch(arm64)
        return "aarch64"
        #else
        return "x86_64"
        #endif
    }
}

struct Message: Identifiable {
    let text: String
    var id: String { text }
}
