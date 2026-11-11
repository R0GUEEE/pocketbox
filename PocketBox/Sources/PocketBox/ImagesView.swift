import SwiftUI

struct ImagesView: View {
    @EnvironmentObject private var model: AppModel
    @State private var pulling = false
    @State private var reference = "alpine:3.20"
    @State private var output: CommandResult?
    @State private var filter = ""

    private var filtered: [ImageRow] {
        guard !filter.isEmpty else { return model.images }
        return model.images.filter { $0.reference.localizedCaseInsensitiveContains(filter) }
    }

    var body: some View {
        NavigationStack {
            Group {
                if model.images.isEmpty {
                    ContentUnavailableView {
                        Label("No images", systemImage: "square.stack.3d.up")
                    } description: {
                        Text("Pull an image from Docker Hub, GHCR or any registry that speaks the OCI distribution API.")
                    } actions: {
                        Button("Pull an image") { pulling = true }
                            .buttonStyle(.borderedProminent)
                    }
                } else {
                    List {
                        ForEach(filtered) { img in
                            VStack(alignment: .leading, spacing: 4) {
                                Text(img.reference).font(.subheadline.weight(.semibold))
                                HStack(spacing: 6) {
                                    Text(img.digest.shortDigest).font(.caption2.monospaced())
                                    Text("·")
                                    Text(ByteCountFormatter.string(fromByteCount: img.size, countStyle: .file))
                                        .font(.caption2)
                                    Text("·")
                                    Text(img.created).font(.caption2)
                                }
                                .foregroundStyle(.secondary)
                            }
                            .swipeActions {
                                Button(role: .destructive) {
                                    model.removeImage(img)
                                } label: { Label("Delete", systemImage: "trash") }
                            }
                        }
                    }
                    .searchable(text: $filter, placement: .navigationBarDrawer(displayMode: .always))
                    .refreshable { model.refreshImages() }
                }
            }
            .navigationTitle("Images")
            .toolbar {
                ToolbarItem(placement: .primaryAction) {
                    Button { pulling = true } label: { Image(systemName: "arrow.down.circle") }
                        .disabled(model.rootfsState != .ready)
                }
            }
            .sheet(isPresented: $pulling) {
                NavigationStack {
                    Form {
                        Section("Reference") {
                            TextField("name[:tag] or name@sha256:…", text: $reference)
                                .textInputAutocapitalization(.never)
                                .autocorrectionDisabled()
                                .font(.body.monospaced())
                        }
                        Section {
                            Button {
                                pulling = false
                                model.pull(reference)
                            } label: {
                                HStack { Spacer(); Text("Pull").bold(); Spacer() }
                            }
                        } footer: {
                            Text("arm64 images are selected automatically. Layers are stored once and shared between images.")
                        }
                    }
                    .navigationTitle("Pull image")
                    .navigationBarTitleDisplayMode(.inline)
                    .toolbar {
                        ToolbarItem(placement: .cancellationAction) {
                            Button("Cancel") { pulling = false }
                        }
                    }
                }
                .presentationDetents([.medium])
            }
            .sheet(item: $output) { OutputSheet(result: $0) }
        }
    }
}
