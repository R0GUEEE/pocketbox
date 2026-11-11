import SwiftUI

struct ConsoleView: View {
    @EnvironmentObject private var model: AppModel
    @ObservedObject private var engineLog = EngineLog.shared
    @State private var command = ""
    @State private var output: CommandResult?
    @State private var showingLog = false
    @FocusState private var focused: Bool

    private let suggestions = [
        "ps -a", "images", "info", "pull alpine:3.20",
        "run -d --name web -p 8080:80 nginx:alpine",
        "logs web", "stop web", "rm web", "version",
    ]

    var body: some View {
        NavigationStack {
            VStack(spacing: 0) {
                if showingLog {
                    ScrollViewReader { proxy in
                        ScrollView {
                            LazyVStack(alignment: .leading, spacing: 2) {
                                ForEach(Array(engineLog.lines.enumerated()), id: \.offset) { i, line in
                                    Text(line)
                                        .font(.system(.caption2, design: .monospaced))
                                        .frame(maxWidth: .infinity, alignment: .leading)
                                        .id(i)
                                }
                            }
                            .padding(10)
                        }
                        .onChange(of: engineLog.lines.count) { _ in
                            withAnimation { proxy.scrollTo(engineLog.lines.count - 1, anchor: .bottom) }
                        }
                        .background(Color(.secondarySystemBackground))
                    }
                } else {
                    ScrollView {
                        VStack(alignment: .leading, spacing: 12) {
                            ForEach(model.history) { item in
                                Button { output = item } label: {
                                    CommandRow(result: item)
                                }
                                .buttonStyle(.plain)
                            }
                            if model.history.isEmpty {
                                Placeholder(icon: "terminal", title: "No commands yet",
                                            message: "Everything you run from this tab executes inside the Linux guest, exactly like the docker CLI would.")
                            }
                        }
                        .padding()
                    }
                }

                Divider()
                HStack(spacing: 8) {
                    TextField("pocketbox ps -a", text: $command)
                        .font(.system(.subheadline, design: .monospaced))
                        .textInputAutocapitalization(.never)
                        .autocorrectionDisabled()
                        .focused($focused)
                        .onSubmit(send)
                        .submitLabel(.go)
                    Button(action: send) {
                        Image(systemName: "arrow.up.circle.fill").font(.title2)
                    }
                    .disabled(command.trimmingCharacters(in: .whitespaces).isEmpty || model.busy)
                }
                .padding(.horizontal, 12)
                .padding(.vertical, 8)
                .background(.bar)

                ScrollView(.horizontal, showsIndicators: false) {
                    HStack(spacing: 8) {
                        ForEach(suggestions, id: \.self) { s in
                            Button(s) { command = s; send() }
                                .font(.caption.monospaced())
                                .buttonStyle(.bordered)
                                .controlSize(.small)
                        }
                    }
                    .padding(.horizontal, 12)
                    .padding(.bottom, 8)
                }
                .background(.bar)
            }
            .navigationTitle("Console")
            .toolbar {
                ToolbarItem(placement: .primaryAction) {
                    Button { showingLog.toggle() } label: {
                        Image(systemName: showingLog ? "list.bullet.rectangle" : "text.justify.leading")
                    }
                }
                ToolbarItem(placement: .topBarLeading) {
                    Menu {
                        Button("Clear history") { model.history.removeAll() }
                        Button("Clear engine log") { engineLog.clear() }
                    } label: { Image(systemName: "trash") }
                }
            }
            .sheet(item: $output) { OutputSheet(result: $0) }
        }
    }

    private func send() {
        let cmd = command.trimmingCharacters(in: .whitespacesAndNewlines)
        guard !cmd.isEmpty else { return }
        model.runCommand(cmd)
        command = ""
    }
}

struct CommandRow: View {
    let result: CommandResult
    var body: some View {
        VStack(alignment: .leading, spacing: 4) {
            HStack(spacing: 6) {
                Image(systemName: result.ok ? "checkmark.circle.fill" : "xmark.octagon.fill")
                    .foregroundStyle(result.ok ? .green : .orange)
                    .font(.caption)
                Text("$ " + result.title)
                    .font(.system(.caption, design: .monospaced).weight(.semibold))
                    .lineLimit(1)
                Spacer()
                Text("\(result.exitCode)").font(.caption2.monospaced()).foregroundStyle(.secondary)
            }
            let preview = (result.stdout + result.stderr).trimmingCharacters(in: .whitespacesAndNewlines)
            if !preview.isEmpty {
                Text(preview.split(separator: "\n").prefix(4).joined(separator: "\n"))
                    .font(.system(.caption2, design: .monospaced))
                    .foregroundStyle(.secondary)
                    .lineLimit(4)
            }
        }
        .frame(maxWidth: .infinity, alignment: .leading)
        .padding(10)
        .background(Color(.secondarySystemBackground), in: RoundedRectangle(cornerRadius: 10))
    }
}
