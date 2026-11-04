import SwiftUI
import AVFoundation
import Mobile

// FreeProxy for iOS (sideload). Made for hihellohejo.

final class KeepAlive {
    static let shared = KeepAlive()
    private var player: AVAudioPlayer?

    // Plays silent audio so iOS keeps the proxy running while you're in Minecraft.
    func start() {
        let s = AVAudioSession.sharedInstance()
        try? s.setCategory(.playback, mode: .default, options: [.mixWithOthers])
        try? s.setActive(true)
        if player == nil {
            player = try? AVAudioPlayer(data: KeepAlive.silentWav())
            player?.numberOfLoops = -1
            player?.volume = 0
        }
        player?.play()
        NotificationCenter.default.addObserver(forName: AVAudioSession.interruptionNotification, object: nil, queue: .main) { [weak self] _ in
            try? AVAudioSession.sharedInstance().setActive(true)
            self?.player?.play()
        }
    }

    func stop() { player?.stop() }

    static func silentWav(seconds: Int = 2, rate: Int = 8000) -> Data {
        let samples = seconds * rate
        var d = Data()
        func u32(_ v: UInt32) { var x = v.littleEndian; d.append(Data(bytes: &x, count: 4)) }
        func u16(_ v: UInt16) { var x = v.littleEndian; d.append(Data(bytes: &x, count: 2)) }
        d.append("RIFF".data(using: .ascii)!); u32(UInt32(36 + samples * 2))
        d.append("WAVEfmt ".data(using: .ascii)!); u32(16); u16(1); u16(1)
        u32(UInt32(rate)); u32(UInt32(rate * 2)); u16(2); u16(16)
        d.append("data".data(using: .ascii)!); u32(UInt32(samples * 2))
        d.append(Data(count: samples * 2))
        return d
    }
}

final class ProxyModel: NSObject, ObservableObject, MobileLoggerProtocol {
    @Published var log = ""
    @Published var running = false
    @Published var loginURL: URL?
    @Published var loginCode: String?
    @Published var choices: [String] = []

    let port = 19133
    var dir: String {
        FileManager.default.urls(for: .documentDirectory, in: .userDomainMask)[0].path
    }

    func output(_ text: String?) {
        guard let text = text else { return }
        DispatchQueue.main.async {
            self.log += text
            if self.log.count > 60000 { self.log = String(self.log.suffix(40000)) }
            self.parse(text)
            self.running = MobileRunning()
        }
    }

    private func parse(_ text: String) {
        // Find the Microsoft login link and code.
        for word in text.split(whereSeparator: { $0 == " " || $0 == "\n" }) {
            let w = String(word).trimmingCharacters(in: CharacterSet(charactersIn: ".,()"))
            if w.hasPrefix("https://"), loginURL == nil, w.contains("microsoft") || w.contains("live") || w.contains("aka.ms") {
                loginURL = URL(string: w)
            }
        }
        if let r = text.range(of: "code ") {
            let rest = text[r.upperBound...].split(separator: " ").first.map(String.init) ?? ""
            let code = rest.trimmingCharacters(in: CharacterSet.alphanumerics.inverted)
            if code.count >= 6, code == code.uppercased() { loginCode = code }
        }
        if text.contains("Signed in!") { loginURL = nil; loginCode = nil }
        // Realm picker lines look like "  1) Realm: Name ..."
        for line in text.split(separator: "\n") {
            let t = line.trimmingCharacters(in: .whitespaces)
            if let p = t.firstIndex(of: ")"), t.distance(from: t.startIndex, to: p) <= 2 {
                let key = String(t[..<p])
                if !choices.contains(where: { $0.hasPrefix(key + ")") }) { choices.append(t) }
            }
        }
        if text.contains("Proxy is running") { choices = [] }
    }

    func start() {
        KeepAlive.shared.start()
        log = ""
        choices = []
        MobileStart(dir, port, self)
        running = true
    }

    func send(_ s: String) {
        log += "> \(s)\n"
        MobileSend(s)
        choices = []
    }
}

struct ContentView: View {
    @StateObject var m = ProxyModel()
    @State var input = ""
    @Environment(\.openURL) var openURL

    var body: some View {
        NavigationView {
            VStack(spacing: 10) {
                HStack {
                    Circle().fill(m.running ? Color.green : Color.gray).frame(width: 12, height: 12)
                    Text(m.running ? "Running — join 127.0.0.1 : \(m.port)" : "Stopped")
                        .font(.subheadline.bold())
                    Spacer()
                }
                if let url = m.loginURL {
                    VStack(spacing: 6) {
                        if let c = m.loginCode {
                            Button("Copy code \(c)") { UIPasteboard.general.string = c }
                                .buttonStyle(.bordered)
                        }
                        Button("Open Microsoft sign-in") { openURL(url) }
                            .buttonStyle(.borderedProminent)
                    }
                }
                if !m.choices.isEmpty {
                    ScrollView(.horizontal) {
                        HStack {
                            ForEach(m.choices, id: \.self) { c in
                                Button(c) {
                                    m.send(String(c.split(separator: ")").first ?? ""))
                                }.buttonStyle(.bordered)
                            }
                        }
                    }
                }
                ScrollViewReader { proxy in
                    ScrollView {
                        Text(m.log.isEmpty ? "Tap Start. First time: sign in, then pick your friend's Realm." : m.log)
                            .font(.system(size: 12, design: .monospaced))
                            .frame(maxWidth: .infinity, alignment: .leading)
                            .textSelection(.enabled)
                        Color.clear.frame(height: 1).id("end")
                    }
                    .onChange(of: m.log) { _ in proxy.scrollTo("end") }
                }
                .padding(8)
                .background(Color(.secondarySystemBackground))
                .cornerRadius(10)
                HStack {
                    TextField("Type here (invite code, IP...)", text: $input)
                        .textFieldStyle(.roundedBorder)
                        .autocapitalization(.none)
                        .disableAutocorrection(true)
                    Button("Send") { m.send(input); input = "" }
                }
                HStack {
                    Button(m.running ? "Running" : "Start") { m.start() }
                        .buttonStyle(.borderedProminent)
                        .disabled(m.running)
                    Spacer()
                    Menu("More") {
                        Button("Change Realm/server") { MobileResetTarget(m.dir); m.log += "\nTarget cleared. Force-close the app and Start again.\n" }
                        Button("Sign out", role: .destructive) { MobileLogout(m.dir); m.log += "\nSigned out. Force-close the app and Start again.\n" }
                    }
                }
                Text("In Minecraft: Servers → Add Server → 127.0.0.1, port \(m.port). Type .help in chat.")
                    .font(.caption).foregroundColor(.secondary)
            }
            .padding()
            .navigationTitle("FreeProxy")
        }
        .navigationViewStyle(.stack)
    }
}

@main
struct FreeProxyApp: App {
    var body: some Scene {
        WindowGroup { ContentView() }
    }
}
