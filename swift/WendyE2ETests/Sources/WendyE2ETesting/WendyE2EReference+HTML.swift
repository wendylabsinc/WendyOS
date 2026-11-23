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
        availability: nil,
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
                "<h3 id=\"\(escapeHTMLAttribute(WendyE2EReference.markdownAnchor(forTitle: title)))\">\(renderInlineHTML(title))\(entry.change.map { " <code>\($0.rawValue)</code>" } ?? "")</h3>"
            )
            appendHTMLMetadata(
                availability: entry.availability,
                availabilityReason: entry.availabilityReason,
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
              --background: #F1EEE7;
              --foreground: #171C23;
              --card: #F1EEE7;
              --panel: #E6E2D8;
              --muted-foreground: #5B5A56;
              --border: #DEDEDE;
              --input: #CBCBCB;
              --seafoam: #9FE2BF;
              --seafoam-hover: #86D3A8;
              --link: #2A7050;
              --primary: #171C23;
              --primary-foreground: #F1EEE7;
              --code-background: #E6E2D8;
              --shadow: rgba(23, 28, 35, .08);
            }

            :root[data-theme="dark"] {
              color-scheme: dark;
              --background: #171C23;
              --foreground: #F1EEE7;
              --card: #1E242D;
              --panel: #242B35;
              --muted-foreground: #C7C2B7;
              --border: rgba(241, 238, 231, .18);
              --input: rgba(241, 238, 231, .32);
              --link: #9FE2BF;
              --primary: #F1EEE7;
              --primary-foreground: #171C23;
              --code-background: #242B35;
              --shadow: rgba(10, 13, 17, .24);
            }

            * { box-sizing: border-box; }

            body {
              margin: 0;
              background: var(--background);
              color: var(--foreground);
              font: 16px/1.6 "Geist", ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
            }

            main {
              max-width: 1120px;
              margin: 0 auto;
              padding: 40px 24px 72px;
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
              flex-wrap: wrap;
              align-items: center;
              gap: 14px 18px;
              margin-bottom: 22px;
            }

            .brand-mark {
              display: inline-flex;
              width: 124px;
              color: var(--foreground);
            }

            .brand-mark svg {
              display: block;
              width: 124px;
              height: auto;
              fill: currentColor;
            }

            .brand-copy {
              padding-left: 18px;
              border-left: 1px solid var(--input);
              color: var(--foreground);
              font: 400 12px/1.35 "Geist Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
              letter-spacing: .1em;
              text-transform: uppercase;
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
              border: 1px solid var(--input);
              border-radius: 999px;
              background: var(--card);
              color: var(--foreground);
              cursor: pointer;
              font: 500 12px/1.4 "Geist Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
              padding: 8px 12px;
            }

            .theme-toggle:hover { background: var(--panel); }

            :where(a, button):focus-visible {
              outline: 3px solid var(--link);
              outline-offset: 3px;
            }

            .theme-toggle-icon {
              color: var(--link);
              font-size: 15px;
              line-height: 1;
            }

            .page-title {
              margin: 0 0 12px;
              max-width: 820px;
              font-size: clamp(36px, 6vw, 56px);
              font-weight: 500;
              line-height: 1.05;
              letter-spacing: 0;
            }

            .lead {
              margin: 0;
              max-width: 720px;
              color: var(--muted-foreground);
              font-size: 16px;
              line-height: 1.55;
            }

            .card {
              margin-top: 40px;
              padding: 28px;
              background: var(--card);
              border: 1px solid var(--border);
              box-shadow: 0 10px 28px var(--shadow);
            }

            h1, h2, h3, h4, h5 {
              color: var(--foreground);
              font-weight: 500;
              letter-spacing: 0;
            }

            .card > h1:first-child {
              margin-top: 0;
              padding-bottom: 10px;
              border-bottom: 1px solid var(--border);
              font-size: 28px;
              line-height: 1.1;
            }

            h2 {
              margin: 28px 0 10px;
              padding-top: 18px;
              border-top: 1px solid var(--border);
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
              color: var(--muted-foreground);
              font: 500 12px/1.4 "Geist Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
              letter-spacing: .08em;
              text-transform: uppercase;
            }

            h5 {
              margin: 12px 0 4px;
              color: var(--muted-foreground);
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
              background: var(--foreground);
            }

            a {
              color: var(--link);
              font-weight: 500;
              text-decoration: none;
            }

            a:hover { text-decoration: underline; }

            code {
              font-family: "Geist Mono", ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
              font-size: .88em;
              background: var(--code-background);
              border: 1px solid var(--border);
              border-radius: 5px;
              padding: .12em .34em;
            }

            pre {
              overflow-x: auto;
              margin: 0 0 16px;
              padding: 1rem;
              background: var(--panel);
              border: 1px solid var(--border);
              border-radius: 12px;
            }

            pre code {
              background: transparent;
              border: 0;
              padding: 0;
            }

            .metadata {
              margin: 8px 0 14px;
              color: var(--muted-foreground);
              font-size: 13px;
            }

            hr {
              border: 0;
              border-top: 1px solid var(--border);
              margin: 2rem 0;
            }

            footer {
              margin-top: 22px;
              color: var(--muted-foreground);
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
                <div class="brand-row">
                  <span class="brand-mark" role="img" aria-label="Wendy"><svg viewBox="0 0 749.97 181.81" role="img" aria-hidden="true"><rect x="91.64" y="26.62" width="128.56" height="128.56" transform="translate(-18.61 136.88) rotate(-45)"/><path d="M69.93,160.83L0,90.9,69.93,20.98l69.93,69.93-69.93,69.93ZM22.63,90.9l47.3,47.3,47.3-47.3-47.3-47.3-47.3,47.3Z"/><path d="M324.28,119.62l-22.55-68.37h21.56l12.51,44.87h.33l15.35-44.87h18l15.3,44.87h.33l12.51-44.87h21.56l-22.55,68.37h-21.18l-14.88-41.98h-.28l-14.83,41.98h-21.18Z"/><path d="M426.43,119.62V51.25h64.01v14.88h-43.26v12.27h40.27v13.41h-40.27v12.93h43.26v14.88h-64.01Z"/><path d="M500.38,119.62V51.25h17.81l48.42,50.22-17.96-9.57h8.53v-40.65h20.18v68.37h-17.77l-48.37-50.74,17.91,9.71h-8.62v41.03h-20.14Z"/><path d="M587.98,119.62V51.25h38.56c8.62,0,15.93,1.3,21.91,3.91,5.99,2.61,10.52,6.42,13.62,11.44,3.09,5.02,4.64,11.15,4.64,18.38v.09c0,7.42-1.56,13.7-4.67,18.83-3.11,5.13-7.66,9.03-13.64,11.7-5.99,2.67-13.27,4-21.87,4h-38.56ZM608.74,104.12h16.39c4.39,0,8.11-.68,11.16-2.04,3.05-1.36,5.37-3.41,6.96-6.16,1.59-2.75,2.39-6.25,2.39-10.52v-.09c0-4.04-.77-7.44-2.3-10.19-1.53-2.75-3.82-4.83-6.87-6.25-3.05-1.42-6.83-2.13-11.35-2.13h-16.39v37.38Z"/><path d="M697.66,119.62v-23.31l-31.55-45.06h22.79l19,28.52h.28l19-28.52h22.79l-31.55,45.06v23.31h-20.75Z"/></svg></span>
                  <span class="brand-copy">Swift E2E · Behavioral reference</span>
                </div>
                <h1 class="page-title">\(escapeHTMLText(plainTitle))</h1>
                <p class="lead">Expected CLI behavior generated from the Swift E2E specifications.</p>
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

            <footer>Generated with <code>swift-e2e-testing reference</code>.</footer>
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
    availability: WendyE2EReference.Availability?,
    availabilityReason: String? = nil,
    sourceLocation: WendyE2EReference.SourceLocation,
    options: WendyE2EReference.RenderOptions,
    to html: inout [String]
) {
    var metadata: [String] = []
    if options.includeDisabledState, let availability {
        metadata.append(availability.rawValue)
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
    if options.includeDisabledState, let availabilityReason {
        html.append(
            "<p class=\"metadata\">Availability traits: <code>\(escapeHTMLText(availabilityReason))</code></p>"
        )
    }
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
