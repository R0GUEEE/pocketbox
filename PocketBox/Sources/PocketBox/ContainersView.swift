import SwiftUI

struct ContainersView: View {
    @EnvironmentObject private var model: AppModel
    @State private var creating = false
    @State private var output: CommandResult?
    @State private var logsFor: ContainerRow?
    @State private var logsData: Data = Data()
    @State private var confirmRemove: ContainerRow?

    var body: some View {
        NavigationStack {
            Group {
                if model.rootfsState == .absent {
                    ContentUnavailableView {
                        Label("No engine yet", systemImage: "shippingbox")
                    } description: {
                        Text("PocketBox downloads a small Linux guest, then runs containers inside it.")
                    } actions: {
                        Button("Prepare guest") { model.prepareRootfs() }
                            .buttonStyle(.borderedProminent)
                    }
                } else if model.containers.isEmpty {
                    ContentUnavailableView {
                        Label("No containers", systemImage: "shippingbox.and.arrow.backward")
                    } description: {
                        Text("Run an image to see it here. Try alpine for a shell, or nginx for a web server.")
                    } actions: {
                        Button("Run a container") { creating = true }
                            .buttonStyle(.borderedProminent)
                    }
                } else {
                    List {
                        ForEach(model.containers) { row in
                            ContainerCard(row: row, actions: actions(for: row))
                                .listRowInsets(.init(top: 6, leading: 12, bottom: 6, trailing: 12))
                                .listRowSeparator(.hidden)
                        }
                    }
                    .listStyle(.plain)
                    .refreshable { model.refreshContainers() }
                }
            }
            .navigationTitle("Containers")
            .toolbar {
                ToolbarItem(placement: .primaryAction) {
                    Button { creating = true } label: { Image(systemName: "plus") }
                        .disabled(!(model.rootfsState == .ready))
                }
                ToolbarItem(placement: .topBarLeading) {
                    Button { model.refreshContainers() } label: { Image(systemName: "arrow.clockwise") }
                        .disabled(model.busy)
                }
            }
            .overlay {
                if model.busy {
                    ProgressView().controlSize(.large)
                        .padding(18)
                        .background(.ultraThinMaterial, in: RoundedRectangle(cornerRadius: 16))
                }
            }
            .sheet(isPresented: $creating) {
                RunSheet { result in
                    creating = false
                    if let r = result { output = r }
                }
                .environmentObject(model)
            }
            .sheet(item: $output) { OutputSheet(result: $0) }
            .sheet(item: $logsFor) { row in
                LogsSheet(title: row.names, data: logsData)
            }
            .confirmationDialog("Remove \(confirmRemove?.names ?? "")?", isPresented: Binding(
                get: { confirmRemove != nil },
                set: { if !$0 { confirmRemove = nil } }
            ), titleVisibility: .visible) {
                Button("Remove", role: .destructive) {
                    if let row = confirmRemove { model.remove(row) }
                    confirmRemove = nil
                }
                Button("Cancel", role: .cancel) { confirmRemove = nil }
            } message: {
                Text("The container and its writable layer are deleted. Images and volumes stay.")
            }
        }
    }

    private func actions(for row: ContainerRow) -> ContainerActions {
        ContainerActions(
            start: { model.start(row) },
            stop: { model.stop(row) },
            logs: {
                logsData = model.logs(row)
                logsFor = row
            },
            remove: { confirmRemove = row }
        )
    }
}

struct ContainerActions {
    let start: () -> Void
    let stop: () -> Void
    let logs: () -> Void
    let remove: () -> Void
}

struct ContainerCard: View {
    let row: ContainerRow
    let actions: ContainerActions

    var body: some View {
        VStack(alignment: .leading, spacing: 8) {
            HStack(spacing: 8) {
                Circle()
                    .fill(row.running ? Color.green : Color.secondary.opacity(0.5))
                    .frame(width: 8, height: 8)
                Text(row.names).font(.subheadline.weight(.semibold))
                Spacer()
                Text(row.shortID).font(.caption2.monospaced()).foregroundStyle(.secondary)
            }
            HStack(spacing: 6) {
                Image(systemName: "square.stack.3d.up").font(.caption2)
                Text(row.image).font(.caption.monospaced())
                Text("·").foregroundStyle(.tertiary)
                Text(row.status).font(.caption)
            }
            .foregroundStyle(.secondary)
            if !row.command.isEmpty && row.command != "—" {
                Text(row.command)
                    .font(.caption2.monospaced())
                    .foregroundStyle(.tertiary)
                    .lineLimit(1)
            }
            HStack(spacing: 14) {
                Button {
                    row.running ? actions.stop() : actions.start()
                } label: {
                    Label(row.running ? "Stop" : "Start", systemImage: row.running ? "stop.fill" : "play.fill")
                }
                .buttonStyle(.bordered)
                .tint(row.running ? .red : .green)
                .controlSize(.small)
                Button {
                    actions.logs()
                } label: {
                    Label("Logs", systemImage: "text.alignleft")
                }
                .buttonStyle(.bordered)
                .controlSize(.small)
                Spacer()
                Menu {
                    Button("Remove", role: .destructive) { actions.remove() }
                } label: {
                    Image(systemName: "ellipsis.circle")
                }
            }
        }
        .padding(12)
        .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 14))
    }
}

struct LogsSheet: View {
    let title: String
    let data: Data
    @Environment(\.dismiss) private var dismiss

    var body: some View {
        NavigationStack {
            ScrollView([.vertical, .horizontal], showsIndicators: true) {
                Text(String(decoding: data, as: UTF8.self))
                    .font(.system(.caption2, design: .monospaced))
                    .textSelection(.enabled)
                    .frame(maxWidth: .infinity, alignment: .leading)
                    .padding()
            }
            .navigationTitle(title)
            .navigationBarTitleDisplayMode(.inline)
            .toolbar { ToolbarItem(placement: .cancellationAction) { Button("Done") { dismiss() } } }
        }
    }
}

struct RunSheet: View {
    @EnvironmentObject private var model: AppModel
    @Environment(\.dismiss) private var dismiss
    @State private var image = "alpine:3.20"
    @State private var command = ""
    @State private var name = ""
    @State private var detach = true
    @State private var ports = ""
    @State private var env = ""

    let done: (CommandResult?) -> Void

    var body: some View {
        NavigationStack {
            Form {
                Section("Image") {
                    TextField("ghcr.io/owner/app:tag", text: $image)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                }
                Section("Run") {
                    TextField("Command (default: image entrypoint)", text: $command)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    TextField("Name (optional)", text: $name)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    TextField("Ports, e.g. 8080:80", text: $ports)
                        .keyboardType(.numbersAndPunctuation)
                    TextField("Env, e.g. KEY=value,OTHER=1", text: $env)
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                    Toggle("Detached", isOn: $detach)
                }
                Section {
                    Button {
                        run()
                    } label: {
                        HStack {
                            Spacer()
                            Text("Run").bold()
                            Spacer()
                        }
                    }
                    .disabled(image.isEmpty)
                } footer: {
                    Text("The image is pulled automatically when missing. Detached containers keep running after you close this sheet; their output goes to Logs.")
                }
            }
            .navigationTitle("Run a container")
            .navigationBarTitleDisplayMode(.inline)
            .toolbar {
                ToolbarItem(placement: .cancellationAction) { Button("Cancel") { done(nil) } }
            }
        }
    }

    private func run() {
        var args: [String] = ["run"]
        if detach { args.append("-d") }
        if !name.isEmpty { args += ["--name", name] }
        for p in ports.split(separator: ",") {
            let s = p.trimmingCharacters(in: .whitespaces)
            if !s.isEmpty { args += ["-p", s] }
        }
        for e in env.split(separator: ",") {
            let s = e.trimmingCharacters(in: .whitespaces)
            if !s.isEmpty { args += ["-e", s] }
        }
        args.append(image)
        args += CommandLineParser.split(command)
        model.run("run \(image)", "/pb/bin/pocketbox", args) { result in
            done(result)
        }
    }
}
