import Hummingbird
import Logging
import OTel
import Tracing

var config = OTel.Configuration.default
config.serviceName = "HelloHTTP"

// Configure OTLP exporters
config.logs.otlpExporter.endpoint = "http://127.0.0.1:4317"
config.logs.otlpExporter.protocol = .grpc
config.metrics.otlpExporter.endpoint = "http://127.0.0.1:4317"
config.metrics.otlpExporter.protocol = .grpc
config.traces.otlpExporter.endpoint = "http://127.0.0.1:4317"
config.traces.otlpExporter.protocol = .grpc

// Export telemetry data every 5 seconds
config.logs.batchLogRecordProcessor.scheduleDelay = .seconds(5)
config.metrics.exportInterval = .seconds(5)
config.traces.batchSpanProcessor.scheduleDelay = .seconds(5)

let observability = try OTel.bootstrap(configuration: config)

let logger = Logger(label: "HelloHTTP")

// create router and add a single GET /hello route
let router = Router()

// Set up middleware for observability
// TracingMiddleware creates automatic spans for each HTTP request
router.add(middleware: TracingMiddleware())
router.add(middleware: LogRequestsMiddleware(.info))
router.add(middleware: MetricsMiddleware())

// Index page with buttons to trigger various endpoints
router.get("/") { request, _ -> Response in
    let html = """
        <!DOCTYPE html>
        <html>
        <head>
            <title>HelloHTTP - Tracing Demo</title>
            <style>
                body {
                    font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif;
                    max-width: 800px;
                    margin: 50px auto;
                    padding: 20px;
                    background: #f5f5f5;
                }
                h1 { color: #333; }
                h2 { color: #666; margin-top: 30px; }
                .card {
                    background: white;
                    border-radius: 8px;
                    padding: 20px;
                    margin: 15px 0;
                    box-shadow: 0 2px 4px rgba(0,0,0,0.1);
                }
                button {
                    background: #007AFF;
                    color: white;
                    border: none;
                    padding: 10px 20px;
                    border-radius: 6px;
                    cursor: pointer;
                    font-size: 14px;
                    margin: 5px;
                }
                button:hover { background: #0056b3; }
                button.error { background: #dc3545; }
                button.error:hover { background: #c82333; }
                .result {
                    margin-top: 10px;
                    padding: 10px;
                    background: #f8f9fa;
                    border-radius: 4px;
                    font-family: monospace;
                    white-space: pre-wrap;
                }
                .description { color: #666; font-size: 14px; margin-bottom: 10px; }
                input {
                    padding: 8px 12px;
                    border: 1px solid #ddd;
                    border-radius: 4px;
                    font-size: 14px;
                    width: 200px;
                }
            </style>
        </head>
        <body>
            <h1>HelloHTTP - Tracing Demo</h1>
            <p>This demo shows OpenTelemetry tracing with Hummingbird. Each button triggers an endpoint that creates traces.</p>

            <h2>Basic Endpoints</h2>

            <div class="card">
                <h3>GET /hello</h3>
                <p class="description">Simple endpoint with automatic TracingMiddleware span.</p>
                <button onclick="callEndpoint('/hello', 'result1')">Call /hello</button>
                <div id="result1" class="result" style="display:none"></div>
            </div>

            <h2>Custom Spans</h2>

            <div class="card">
                <h3>GET /hello/:name</h3>
                <p class="description">Creates nested child spans: generate-greeting → format-message. Shows span attributes and logging within span context.</p>
                <input type="text" id="name1" placeholder="Enter a name" value="World">
                <button onclick="callEndpoint('/hello/' + document.getElementById('name1').value, 'result2')">Call /hello/:name</button>
                <div id="result2" class="result" style="display:none"></div>
            </div>

            <h2>Error Handling with Traces</h2>

            <div class="card">
                <h3>GET /greet/:name</h3>
                <p class="description">Shows validation with span events and error status. Try empty name or very long name to see error traces.</p>
                <input type="text" id="name2" placeholder="Enter a name" value="Alice">
                <button onclick="callEndpoint('/greet/' + document.getElementById('name2').value, 'result3')">Valid Name</button>
                <button class="error" onclick="callEndpoint('/greet/', 'result3')">Empty Name (Error)</button>
                <button class="error" onclick="callEndpoint('/greet/' + 'x'.repeat(100), 'result3')">Long Name (Error)</button>
                <div id="result3" class="result" style="display:none"></div>
            </div>

            <h2>Load Test</h2>

            <div class="card">
                <h3>Generate Multiple Traces</h3>
                <p class="description">Send multiple requests to generate a batch of traces for visualization.</p>
                <button onclick="loadTest(10)">Send 10 Requests</button>
                <button onclick="loadTest(50)">Send 50 Requests</button>
                <div id="result4" class="result" style="display:none"></div>
            </div>

            <script>
                async function callEndpoint(path, resultId) {
                    const resultDiv = document.getElementById(resultId);
                    resultDiv.style.display = 'block';
                    resultDiv.textContent = 'Loading...';

                    try {
                        const response = await fetch(path);
                        const text = await response.text();
                        resultDiv.textContent = `Status: ${response.status}\\nResponse: ${text}`;
                    } catch (error) {
                        resultDiv.textContent = `Error: ${error.message}`;
                    }
                }

                async function loadTest(count) {
                    const resultDiv = document.getElementById('result4');
                    resultDiv.style.display = 'block';
                    resultDiv.textContent = `Sending ${count} requests...`;

                    const names = ['Alice', 'Bob', 'Charlie', 'Diana', 'Eve', 'Frank', 'Grace', 'Henry'];
                    const endpoints = ['/hello', '/hello/World', '/greet/Test'];

                    let completed = 0;
                    const promises = [];

                    for (let i = 0; i < count; i++) {
                        const endpoint = endpoints[i % endpoints.length];
                        const name = names[i % names.length];
                        const path = endpoint.includes(':') ? endpoint : (endpoint === '/hello' ? endpoint : `/hello/${name}`);

                        promises.push(
                            fetch(endpoint === '/hello' ? '/hello' : `/hello/${name}`)
                                .then(() => { completed++; })
                                .catch(() => { completed++; })
                        );
                    }

                    await Promise.all(promises);
                    resultDiv.textContent = `Completed ${completed}/${count} requests. Check your trace collector!`;
                }
            </script>
        </body>
        </html>
        """
    return Response(
        status: .ok,
        headers: [.contentType: "text/html; charset=utf-8"],
        body: .init(byteBuffer: .init(string: html))
    )
}

// Simple endpoint - TracingMiddleware handles the span automatically
router.get("hello") { request, _ -> String in
    return "Hello"
}

// Endpoint demonstrating custom child spans for detailed tracing
router.get("hello/:name") { request, context -> String in
    let name = try context.parameters.require("name")

    // Create a child span for the greeting logic
    // This appears as a nested span under the HTTP request span
    return try await withSpan("generate-greeting") { span in
        // Add attributes to provide context in traces
        span.attributes["greeting.name"] = name
        span.attributes["greeting.style"] = "formal"

        // Simulate some work (e.g., database lookup, external API call)
        let greeting = try await withSpan("format-message") { innerSpan in
            innerSpan.attributes["message.length"] = name.count

            // Log within the span context - logs are correlated with traces
            logger.info("Generating greeting for user", metadata: ["name": "\(name)"])

            return "Hello, \(name)!"
        }

        span.attributes["greeting.result"] = greeting
        return greeting
    }
}

// Endpoint showing error handling with traces
router.get("greet/:name") { request, context -> String in
    let name = try context.parameters.require("name")

    return try await withSpan("validate-and-greet") { span in
        span.attributes["user.name"] = name

        // Validation with span events
        if name.isEmpty {
            var event = SpanEvent(name: "validation-failed")
            event.attributes["reason"] = "empty-name"
            span.addEvent(event)
            span.setStatus(.init(code: .error, message: "Name cannot be empty"))
            throw HTTPError(.badRequest, message: "Name cannot be empty")
        }

        if name.count > 50 {
            var event = SpanEvent(name: "validation-failed")
            event.attributes["reason"] = "name-too-long"
            span.addEvent(event)
            span.setStatus(.init(code: .error, message: "Name too long"))
            throw HTTPError(.badRequest, message: "Name too long")
        }

        span.addEvent(SpanEvent(name: "validation-passed"))
        span.setStatus(.init(code: .ok))

        return "Greetings, \(name)!"
    }
}

// create application using router
var app = Application(
    router: router,
    configuration: .init(address: .hostname("0.0.0.0", port: 8080))
)
app.addServices(observability)

// run hummingbird application
try await app.runService()
