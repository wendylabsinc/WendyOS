import Foundation

extension WendyE2EReference {
    // MARK: - Rendering HTML

    public static func htmlFileName(forTitle title: String) -> String {
        "\(markdownSlug(forTitle: title, fallback: "reference")).html"
    }

    public static func renderHTML(
        _ documents: [Document],
        options: RenderOptions = .reference
    ) -> String {
        let title = documents.first?.title ?? "Reference"
        let body = documents.map { renderHTMLBody($0, options: options) }
            .joined(separator: "\n<hr>\n")
        return renderHTMLDocument(title: title, body: body)
    }

    public static func renderHTMLIndex(
        _ entries: [IndexEntry],
        title: String = "Reference"
    ) -> String {
        var html: [String] = []
        html.append("<h1>\(renderInlineHTML(title))</h1>")
        html.append("<ul>")
        for entry in entries {
            let target = entry.anchor.map { "\(entry.fileName)#\($0)" } ?? entry.fileName
            html.append(
                "<li><a href=\"\(escapeHTMLAttribute(target))\">\(renderInlineHTML(entry.title))</a></li>"
            )
        }
        html.append("</ul>")
        return renderHTMLDocument(title: title, body: html.joined(separator: "\n"))
    }

    public static func renderHTML(
        _ document: Document,
        options: RenderOptions = .reference
    ) -> String {
        renderHTMLDocument(
            title: document.title,
            body: renderHTMLBody(document, options: options)
        )
    }
}

// MARK: - HTML Rendering

private func renderHTMLBody(
    _ document: WendyE2EReference.Document,
    options: WendyE2EReference.RenderOptions
) -> String {
    var html: [String] = []
    html.append(
        "<h1 id=\"\(escapeHTMLAttribute(WendyE2EReference.markdownAnchor(forTitle: document.title)))\">\(renderInlineHTML(document.title))</h1>"
    )
    appendHTMLBlocks(document.overview, to: &html)
    appendHTMLMetadata(
        isDisabled: nil,
        sourceLocation: document.sourceLocation,
        options: options,
        to: &html
    )

    for section in document.sections where !section.entries.isEmpty {
        html.append(
            "<h2 id=\"\(escapeHTMLAttribute(WendyE2EReference.markdownAnchor(forTitle: section.title)))\">\(renderInlineHTML(section.title))</h2>"
        )

        for entry in section.entries {
            let title = referenceBehaviorTitle(
                documentTitle: document.title,
                entryTitle: entry.title
            )
            html.append(
                "<h3 id=\"\(escapeHTMLAttribute(WendyE2EReference.markdownAnchor(forTitle: title)))\">\(renderInlineHTML(title))</h3>"
            )
            appendHTMLMetadata(
                isDisabled: entry.isDisabled,
                sourceLocation: entry.sourceLocation,
                options: options,
                to: &html
            )
            appendHTMLBlocks(entry.documentation, to: &html)

        }
    }

    return html.joined(separator: "\n")
}

private func renderHTMLDocument(title: String, body: String) -> String {
    let plainTitle = strippingInlineCodeMarkup(from: title)
    return """
        <!doctype html>
        <html lang="en">
        <head>
          <meta charset="utf-8" />
          <meta name="viewport" content="width=device-width, initial-scale=1" />
          <title>\(escapeHTMLText(plainTitle))</title>
          <script>
            (() => {
              try {
                const stored = localStorage.getItem('wendy-e2e-theme');
                const preferred = matchMedia('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
                document.documentElement.dataset.theme = stored || preferred;
              } catch {
                document.documentElement.dataset.theme = 'light';
              }
            })();
          </script>
          <style>
            :root {
              color-scheme: light;
              --emerald-50: #ecfdf5;
              --emerald-100: #d1fae5;
              --emerald-200: #a7f3d0;
              --emerald-300: #6ee7b7;
              --emerald-400: #34d399;
              --emerald-500: #10b981;
              --emerald-600: #059669;
              --emerald-700: #047857;
              --emerald-800: #065f46;
              --emerald-900: #064e3b;
              --emerald-950: #022c22;

              --bg: #f8fafc;
              --card: rgba(255, 255, 255, .92);
              --panel: rgba(255, 255, 255, .78);
              --text: #111827;
              --muted: #64748b;
              --line: #e5e7eb;
              --soft: #f3f4f6;
              --blue: var(--emerald-600);
              --shadow: rgba(15, 23, 42, .08);
              --shadow-strong: rgba(15, 23, 42, .14);
              --focus-ring: rgba(16, 185, 129, .18);
              --code-bg: rgba(243, 244, 246, .90);
            }

            :root[data-theme="dark"] {
              color-scheme: dark;
              --bg: #020617;
              --card: rgba(15, 23, 42, .88);
              --panel: rgba(30, 41, 59, .58);
              --text: #f8fafc;
              --muted: #94a3b8;
              --line: rgba(148, 163, 184, .22);
              --soft: rgba(51, 65, 85, .48);
              --blue: var(--emerald-400);
              --shadow: rgba(0, 0, 0, .28);
              --shadow-strong: rgba(0, 0, 0, .38);
              --focus-ring: rgba(52, 211, 153, .22);
              --code-bg: rgba(51, 65, 85, .62);
            }

            * { box-sizing: border-box; }

            body {
              margin: 0;
              background: var(--bg);
              color: var(--text);
              font: 16px/1.6 Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            }

            main {
              max-width: 1080px;
              margin: 0 auto;
              padding: 28px 24px 72px;
            }

            header {
              display: grid;
              grid-template-columns: minmax(0, 1fr) auto;
              gap: 20px;
              align-items: end;
              margin-bottom: 18px;
            }

            .brand-row {
              display: inline-flex;
              align-items: center;
              gap: 10px;
              margin-bottom: 14px;
            }

            .brand-mark {
              display: inline-grid;
              place-items: center;
              width: 34px;
              height: 34px;
              color: var(--text);
            }

            .brand-mark svg {
              display: block;
              width: 30px;
              height: 30px;
              fill: currentColor;
            }

            .brand-copy {
              display: grid;
              gap: 0;
              line-height: 1.1;
            }

            .brand-copy strong {
              font-size: 15px;
              letter-spacing: -.02em;
            }

            .brand-copy span {
              color: var(--muted);
              font-size: 12px;
              font-weight: 700;
              text-transform: uppercase;
              letter-spacing: .08em;
            }

            .header-side {
              display: grid;
              gap: 10px;
              justify-items: end;
            }

            .theme-toggle {
              appearance: none;
              display: inline-flex;
              align-items: center;
              gap: 8px;
              border: 1px solid var(--line);
              border-radius: 999px;
              background: var(--card);
              color: var(--text);
              cursor: pointer;
              font: inherit;
              font-size: 13px;
              font-weight: 900;
              padding: 8px 12px;
              box-shadow: 0 8px 22px var(--shadow);
            }

            .theme-toggle:hover {
              transform: translateY(-1px);
              box-shadow: 0 10px 26px var(--shadow-strong);
            }

            .theme-toggle:focus-visible {
              outline: 3px solid var(--focus-ring);
              outline-offset: 2px;
            }

            .theme-toggle-icon {
              color: var(--blue);
              font-size: 15px;
              line-height: 1;
            }

            .page-title {
              margin: 0 0 8px;
              font-size: clamp(28px, 4vw, 40px);
              line-height: 1.04;
              letter-spacing: -0.045em;
            }

            .lead {
              margin: 0;
              max-width: 720px;
              color: var(--muted);
              font-size: 15px;
              line-height: 1.45;
            }

            .card {
              margin-top: 30px;
              padding: 22px;
              background: var(--card);
              border: 1px solid var(--line);
              border-radius: 18px;
              box-shadow: 0 10px 28px var(--shadow);
            }

            h1, h2, h3, h4, h5 {
              color: var(--text);
              letter-spacing: -0.025em;
            }

            .card > h1:first-child {
              margin-top: 0;
              padding-bottom: 10px;
              border-bottom: 1px solid var(--line);
              font-size: 28px;
              line-height: 1.1;
            }

            h2 {
              margin: 28px 0 10px;
              padding-top: 18px;
              border-top: 1px solid var(--line);
              font-size: 22px;
              line-height: 1.2;
            }

            h3 {
              margin: 18px 0 7px;
              font-size: 18px;
              line-height: 1.35;
            }

            h4 {
              margin: 16px 0 8px;
              color: var(--muted);
              font-size: 12px;
              font-weight: 900;
              letter-spacing: .07em;
              text-transform: uppercase;
            }

            h5 {
              margin: 12px 0 4px;
              color: var(--muted);
              font-size: 13px;
            }

            p { margin: 0 0 12px; }

            ul {
              margin: 0 0 16px;
              padding: 0;
              list-style: none;
            }

            li {
              position: relative;
              padding: 3px 0 3px 20px;
            }

            li::before {
              content: "";
              position: absolute;
              left: 2px;
              top: .85em;
              width: 6px;
              height: 6px;
              border-radius: 999px;
              background: var(--blue);
            }

            a {
              color: var(--blue);
              font-weight: 800;
              text-decoration: none;
            }

            a:hover { text-decoration: underline; }

            code {
              font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
              font-size: .88em;
              background: var(--code-bg);
              border: 1px solid var(--line);
              border-radius: 5px;
              padding: .12em .34em;
            }

            pre {
              overflow-x: auto;
              margin: 0 0 16px;
              padding: 1rem;
              background: var(--soft);
              border: 1px solid var(--line);
              border-radius: 12px;
            }

            pre code {
              background: transparent;
              border: 0;
              padding: 0;
            }

            .metadata {
              margin: 8px 0 14px;
              color: var(--muted);
              font-size: 13px;
            }

            hr {
              border: 0;
              border-top: 1px solid var(--line);
              margin: 2rem 0;
            }

            footer {
              margin-top: 22px;
              color: var(--muted);
              font-size: 13px;
              text-align: center;
            }

            @media (max-width: 680px) {
              main { padding: 24px 16px 56px; }
              header { grid-template-columns: 1fr; }
              .header-side { justify-items: stretch; }
              .theme-toggle { justify-self: start; }
              .card { padding: 16px; }
            }
          </style>
        </head>
        <body>
          <main>
            <header>
              <div>
                <div class="brand-row" aria-label="Wendy E2E Reference">
                  <span class="brand-mark" aria-hidden="true"><svg viewBox="0 0 1024 1024" role="img"><rect x="407.04" y="299.64" width="424.72" height="424.72" transform="translate(-180.62 587.94) rotate(-45)"/><path d="M335.3,743.03l-231.03-231.03,231.03-231.02,231.02,231.02-231.02,231.03ZM179.04,512l156.27,156.27,156.27-156.27-156.27-156.27-156.27,156.27Z"/></svg></span>
                  <span class="brand-copy"><strong>E2E Reference</strong><span>Swift Specs</span></span>
                </div>
                <h1 class="page-title">\(escapeHTMLText(plainTitle))</h1>
                <p class="lead">Behavioral reference generated from Swift E2E specs.</p>
              </div>
              <div class="header-side">
                <button class="theme-toggle" type="button" data-theme-toggle aria-label="Switch color theme">
                  <span class="theme-toggle-icon" data-theme-toggle-icon aria-hidden="true">◐</span>
                  <span data-theme-toggle-label>Theme</span>
                </button>
              </div>
            </header>

            <section class="card">
        \(body)
            </section>

            <footer>Generated by <code>swift-e2e-testing reference</code></footer>
          </main>
          <script>
            (() => {
              const themeToggle = document.querySelector('[data-theme-toggle]');
              const themeToggleIcon = document.querySelector('[data-theme-toggle-icon]');
              const themeToggleLabel = document.querySelector('[data-theme-toggle-label]');

              function currentTheme() {
                return document.documentElement.dataset.theme === 'dark' ? 'dark' : 'light';
              }

              function updateThemeToggle() {
                const theme = currentTheme();
                if (themeToggle) {
                  themeToggle.setAttribute('aria-pressed', String(theme === 'dark'));
                  themeToggle.setAttribute('title', `Switch to ${theme === 'dark' ? 'light' : 'dark'} mode`);
                }
                if (themeToggleIcon) themeToggleIcon.textContent = theme === 'dark' ? '☾' : '☼';
                if (themeToggleLabel) themeToggleLabel.textContent = theme === 'dark' ? 'Dark' : 'Light';
              }

              function setTheme(theme) {
                document.documentElement.dataset.theme = theme;
                try { localStorage.setItem('wendy-e2e-theme', theme); } catch {}
                updateThemeToggle();
              }

              themeToggle?.addEventListener('click', () => {
                setTheme(currentTheme() === 'dark' ? 'light' : 'dark');
              });

              updateThemeToggle();
            })();
          </script>
        </body>
        </html>
        """
}

private func appendHTMLBlocks(_ text: String, to html: inout [String]) {
    let lines = text.trimmingCharacters(in: .whitespacesAndNewlines).components(
        separatedBy: .newlines
    )
    guard !lines.isEmpty, !(lines.count == 1 && lines[0].isEmpty) else {
        return
    }

    var paragraph: [String] = []
    var listItems: [String] = []
    var codeLines: [String] = []
    var isInCodeFence = false

    func flushParagraph() {
        guard !paragraph.isEmpty else { return }
        let text = paragraph.map { $0.trimmingCharacters(in: .whitespaces) }.joined(separator: " ")
        html.append("<p>\(renderInlineHTML(text))</p>")
        paragraph.removeAll()
    }

    func flushList() {
        guard !listItems.isEmpty else { return }
        html.append("<ul>")
        for item in listItems {
            html.append("<li>\(renderInlineHTML(item))</li>")
        }
        html.append("</ul>")
        listItems.removeAll()
    }

    func flushCode() {
        guard !codeLines.isEmpty else { return }
        html.append("<pre><code>\(escapeHTMLText(codeLines.joined(separator: "\n")))</code></pre>")
        codeLines.removeAll()
    }

    for line in lines {
        let trimmed = line.trimmingCharacters(in: .whitespaces)
        if trimmed.hasPrefix("```") {
            if isInCodeFence {
                flushCode()
                isInCodeFence = false
            } else {
                flushParagraph()
                flushList()
                isInCodeFence = true
            }
            continue
        }

        if isInCodeFence {
            codeLines.append(line)
        } else if trimmed.isEmpty {
            flushParagraph()
            flushList()
        } else if let listItem = trimmed.removingPrefix("-") {
            flushParagraph()
            listItems.append(listItem)
        } else {
            flushList()
            paragraph.append(line)
        }
    }

    flushParagraph()
    flushList()
    flushCode()
}

private func appendHTMLMetadata(
    isDisabled: Bool?,
    sourceLocation: WendyE2EReference.SourceLocation,
    options: WendyE2EReference.RenderOptions,
    to html: inout [String]
) {
    var metadata: [String] = []
    if options.includeDisabledState, let isDisabled {
        metadata.append(isDisabled ? "disabled" : "enabled")
    }
    if options.includeSourceLocations {
        metadata.append(
            "<code>\(escapeHTMLText("\(sourceLocation.path):\(sourceLocation.line)"))</code>"
        )
    }

    guard !metadata.isEmpty else {
        return
    }

    html.append("<p class=\"metadata\">\(metadata.joined(separator: " · "))</p>")
}

private func renderInlineHTML(_ value: String) -> String {
    var html = ""
    var cursor = value.startIndex

    while cursor < value.endIndex {
        guard value[cursor] == "`" else {
            html.append(escapeHTMLText(String(value[cursor])))
            cursor = value.index(after: cursor)
            continue
        }

        let contentStart = value.index(after: cursor)
        guard let contentEnd = value[contentStart...].firstIndex(of: "`") else {
            html.append(escapeHTMLText(String(value[cursor])))
            cursor = value.index(after: cursor)
            continue
        }

        html.append("<code>")
        html.append(escapeHTMLText(String(value[contentStart..<contentEnd])))
        html.append("</code>")
        cursor = value.index(after: contentEnd)
    }

    return html
}

private func escapeHTMLText(_ value: String) -> String {
    value
        .replacingOccurrences(of: "&", with: "&amp;")
        .replacingOccurrences(of: "<", with: "&lt;")
        .replacingOccurrences(of: ">", with: "&gt;")
}

private func escapeHTMLAttribute(_ value: String) -> String {
    escapeHTMLText(value)
        .replacingOccurrences(of: "\"", with: "&quot;")
}

private func strippingInlineCodeMarkup(from value: String) -> String {
    value.replacingOccurrences(of: "`", with: "")
}
