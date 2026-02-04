import Hummingbird
import OTel

var config = OTel.Configuration.default
config.serviceName = "HelloHTTP"
config.logs.otlpExporter.endpoint = "http://127.0.0.1:4317"
config.logs.otlpExporter.protocol = .grpc
config.metrics.otlpExporter.endpoint = "http://127.0.0.1:4317"
config.metrics.otlpExporter.protocol = .grpc
config.traces.otlpExporter.endpoint = "http://127.0.0.1:4317"
config.traces.otlpExporter.protocol = .grpc

let observability = try OTel.bootstrap(configuration: config)

// create router and add a single GET /hello route
let router = Router()

// Set up middleware for observability
router.add(middleware: TracingMiddleware())
router.add(middleware: LogRequestsMiddleware(.info))
router.add(middleware: MetricsMiddleware())

router.get("hello") { request, _ -> String in
    return "Hello"
}
adasdsad this does not compile
// create application using router
var app = Application(
    router: router,
    configuration: .init(address: .hostname("0.0.0.0", port: 8080))
)
app.addServices(observability)

// run hummingbird application
try await app.runService()
