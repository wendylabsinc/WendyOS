import Foundation

func escapeHTML(_ text: String) -> String {
    text.replacingOccurrences(of: "&", with: "&amp;")
        .replacingOccurrences(of: "<", with: "&lt;")
        .replacingOccurrences(of: ">", with: "&gt;")
        .replacingOccurrences(of: "\"", with: "&quot;")
        .replacingOccurrences(of: "'", with: "&#39;")
}

func renderPage(form: NotificationForm, outcome: SendOutcome?, records: [SendRecord],
                limits: LimitSnapshot, now: Date = Date()) -> String {
    let e = escapeHTML
    let roles = [(1, "Owner"), (2, "Admin"), (3, "Billing manager"), (4, "Member"), (5, "Viewer")]
        .map { value, label in
            "<label class=\"check\"><input type=\"checkbox\" name=\"roles\" value=\"\(value)\"\(form.roles.contains(value) ? " checked" : "")> \(label)</label>"
        }.joined()
    let severities = [("1", "Info"), ("2", "Warning"), ("3", "Error"), ("4", "Critical")]
        .map { value, label in
            "<option value=\"\(value)\"\(form.severity == value ? " selected" : "")>\(label)</option>"
        }.joined()
    let result = outcome.map {
        """
        <section class="result" aria-labelledby="result-title">
          <h2 id="result-title">\($0.created ? "Notification created" : "Notification refused")</h2>
          <p class="mono">\(e($0.code))</p><pre>\(e($0.message))</pre>
          \($0.underlyingCause.map { "<p>Underlying connection error</p><pre>\(e($0))</pre>" } ?? "")
        </section>
        """
    } ?? ""
    let rows = records.map { record in
        """
        <tr><td><time>\(e(record.time.ISO8601Format()))</time></td>
        <td>\(e(record.title))<br><code>\(e(record.outcome.notificationID))</code></td>
        <td><strong>\(e(record.outcome.code))</strong><pre>\(e(record.outcome.message))</pre></td></tr>
        """
    }.joined()
    func countdown(_ date: Date, label: String) -> String {
        let seconds = max(0, Int(ceil(date.timeIntervalSince(now))))
        return "\(label) <span data-seconds=\"\(seconds)\">\(seconds)s</span>"
    }
    let state: String
    let timing: String
    if let until = limits.quarantineUntil {
        state = "refused"
        timing = "Cloud reports quarantine. " + countdown(until, label: "Retry in")
            + "<br>Reported end: \(e(until.ISO8601Format())). Reload before retrying."
    } else if let retry = limits.retryAt {
        state = "refused"
        timing = "Cloud refused the minute limit. " + countdown(retry, label: "Retry in")
            + "<br>Reported retry time: \(e(retry.ISO8601Format()))."
    } else {
        state = limits.refused ? "refused" : (limits.remaining <= 3 ? "warning" : "calm")
        let reset = limits.resetAt.map { countdown($0, label: "Estimated window reset in") }
            ?? "Window estimate starts with the first accepted send."
        timing = (limits.refused ? "A rate-limit refusal was reported. " : "") + reset
    }
    return """
    <!doctype html>
    <html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
    <meta name="color-scheme" content="light dark"><title>WendyNotify</title>
    <link rel="preconnect" href="https://fonts.googleapis.com"><link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
    <link href="https://fonts.googleapis.com/css2?family=Geist:wght@400;500&amp;family=Geist+Mono:wght@400;500&amp;display=swap" rel="stylesheet">
    <style>
    :root{color-scheme:light dark;--paper:#F1EEE7;--ink:#171C23;--surface:#E6E2D8;--muted:#5B5A56;--line:#CBCBCB;--link:#2A7050;--warning:#825500;--refused:#A13025}
    *{box-sizing:border-box;border-radius:0}body{margin:0;background:var(--paper);color:var(--ink);font:16px/1.5 'Geist',system-ui,sans-serif}
    main{max-width:1056px;margin:auto;padding:40px 24px}header{display:flex;gap:20px;align-items:center;margin-bottom:36px}
    .emblem{width:58px;height:auto;fill:var(--ink);flex:none}h1,h2,strong{font-weight:500}h1{font-size:clamp(36px,6vw,60px);line-height:1.05;margin:0}h2{font-size:24px;margin:0 0 16px}
    .eyebrow,.mono,code,time{font-family:'Geist Mono',ui-monospace,monospace}.eyebrow{font-size:13px;text-transform:uppercase;letter-spacing:.04em}
    p{margin:12px 0}.muted,small{color:var(--muted)}a{color:var(--link)}.layout{display:grid;grid-template-columns:minmax(0,2fr) minmax(240px,1fr);gap:28px;align-items:start}
    section,aside,fieldset{border:1px solid var(--line);padding:24px}fieldset{margin:0 0 24px;padding:16px}legend{padding:0 8px}section{margin-bottom:24px}.result{background:var(--surface)}
    label{display:block;margin:0 0 20px}label span{display:block;margin-bottom:6px}input,textarea,select,button{font:inherit;color:inherit;border:1px solid var(--line);padding:10px 12px;background:var(--paper)}
    input:not([type=checkbox]),textarea,select{width:100%}textarea{resize:vertical;min-height:96px}input[name=notificationID],textarea[name=metadata]{font-family:'Geist Mono',ui-monospace,monospace;font-size:14px}
    .check{display:inline-flex;align-items:center;gap:6px;margin:4px 16px 4px 0}.check input{width:18px;height:18px;accent-color:var(--ink)}
    button{background:#9FE2BF;color:#171C23;border-color:#171C23;cursor:pointer;padding:12px 32px}button:hover{background:#86D3A8}:focus-visible{outline:2px solid var(--ink);outline-offset:3px}
    .budget{border-top:5px solid var(--ink)}.budget.warning{border-top-color:var(--warning)}.budget.refused{border-top-color:var(--refused)}.budget.warning h2{color:var(--warning)}.budget.refused h2{color:var(--refused)}
    meter::-webkit-meter-optimum-value{background:var(--ink)}meter::-webkit-meter-suboptimum-value{background:var(--warning)}meter::-webkit-meter-even-less-good-value{background:var(--refused)}meter::-moz-meter-bar{background:var(--ink)}meter{display:block;width:100%;height:22px;margin:12px 0;accent-color:var(--ink)}.limits{padding-left:20px;font-size:14px}.limits li{margin-bottom:10px}
    pre{font:14px/1.6 'Geist Mono',ui-monospace,monospace;white-space:pre-wrap;overflow-wrap:anywhere;margin:8px 0}code{font-size:12px;overflow-wrap:anywhere}.table-scroll{overflow-x:auto}table{border-collapse:collapse;width:100%;font-size:14px}th,td{text-align:left;padding:14px 12px;border-bottom:1px solid var(--line);vertical-align:top}th{font-weight:500}time{font-size:12px}
    @media(prefers-color-scheme:dark){:root{--paper:#171C23;--ink:#F1EEE7;--surface:#1E242D;--muted:#C1BEB6;--line:#535860;--link:#9FE2BF;--warning:#EAC477;--refused:#F2A69C}input,textarea,select{background:#242B35}}
    @media(max-width:740px){.layout{grid-template-columns:1fr}main{padding:24px 16px}section,aside{padding:18px}}
    </style></head><body><main>
    <header><svg class="emblem" xmlns="http://www.w3.org/2000/svg" viewBox="0 0 246.82 181.81" aria-label="Wendy" role="img"><rect x="91.64" y="26.62" width="128.56" height="128.56" transform="translate(-18.61 136.88) rotate(-45)"/><path d="M69.93,160.83L0,90.9,69.93,20.98l69.93,69.93-69.93,69.93ZM22.63,90.9l47.3,47.3,47.3-47.3-47.3-47.3-47.3,47.3Z"/></svg>
    <div><p class="eyebrow">WendyOS / Examples</p><h1>WendyNotify</h1></div></header>
    <p>Create a notification from this device. The Agent checks your request and Cloud creates it for the selected recipients.</p>
    \(result)
    <div class="layout"><section><h2>Compose notification</h2>
    <form method="post" action="/" novalidate>
    <label><span>Recipient user IDs</span><textarea name="userIDs" aria-describedby="audience-hint">\(e(form.userIDs))</textarea></label>
    <fieldset><legend>Organization roles</legend>\(roles)</fieldset>
    <p id="audience-hint" class="muted">One user ID per line. Recipients are the union of user IDs and checked roles.</p>
    <label><span>Title</span><input name="title" value="\(e(form.title))"></label>
    <label><span>Body</span><textarea name="body">\(e(form.body))</textarea></label>
    <label><span>Severity</span><select name="severity">\(severities)</select></label>
    <label><span>Deep link</span><input name="deepLink" value="\(e(form.deepLink))"></label>
    <label><span>Notification ID · UUID v4</span><input name="notificationID" value="\(e(form.notificationID))"></label>
    <label><span>Metadata JSON · optional object</span><textarea name="metadata" placeholder="{}">\(e(form.metadata))</textarea></label>
    <button type="submit">Send notification</button>
    <p><a href="/">Start a new notification</a></p>
    </form><p class="muted">Team selectors are unavailable because Cloud v2 refuses them.</p></section>
    <aside class="budget \(state)" aria-labelledby="budget-title"><p class="eyebrow">Send budget</p>
    <h2 id="budget-title">\(limits.remaining) of 10 left this minute</h2>
    <meter min="0" max="10" low="3" high="7" optimum="10" value="\(limits.remaining)" aria-label="Estimated notifications remaining">\(limits.remaining) of 10</meter>
    <p>\(timing)</p><p class="muted">Estimate from this app’s accepted sends since it started. Other instances and earlier sends are not counted. Reload to update the count. Cloud’s answer is authoritative.</p>
    <h2>Limits</h2><ul class="limits"><li>10 accepted sends per minute per app. The Agent also limits bursts.</li><li>3 Cloud over-limit refusals in 10 minutes cause a 15-minute quarantine and a Wendy Security alert to owners and admins.</li><li>At most 100 selectors and 100 recipients.</li><li>Title: 1–120 UTF-8 bytes. Body: 1–2000 printable UTF-8 bytes.</li><li>Links must target this device, for example <code>wendy://devices/current/live</code>.</li></ul></aside></div>
    <section><h2>Last 10 sends</h2><p class="muted">In memory only. A creation confirms Cloud accepted the notification; it does not confirm a push reached a phone.</p>
    \(records.isEmpty ? "<p>No sends yet.</p>" : "<div class=\"table-scroll\"><table><thead><tr><th>Time · UTC</th><th>Title / ID</th><th>Result</th></tr></thead><tbody>\(rows)</tbody></table></div>")
    </section></main>
    <script>
    for (const node of document.querySelectorAll('[data-seconds]')) {
      const end = performance.now() + Number(node.dataset.seconds) * 1000;
      const tick = () => {
        const seconds = Math.max(0, Math.ceil((end - performance.now()) / 1000));
        node.textContent = seconds > 0 ? `${Math.floor(seconds / 60)}m ${seconds % 60}s` : '0s — reload to check';
      };
      tick(); setInterval(tick, 1000);
    }
    </script></body></html>
    """
}
