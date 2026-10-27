# Kotlin/JVM HelloWorld

Requires Go, a C compiler, Python 3.9+, JDK 21, and Gradle 9.8.1 installed on `PATH`. Set `JAVA_HOME` to your JDK installation. This is a JVM console app, not an Android app.

From `go/wendy-core/`:

```sh
python3 scripts/build.py native
python3 scripts/build.py jni
gradle -p distributions/kotlin jar --no-daemon
gradle -p examples/kotlin run --no-daemon
```

On Windows, use `python` and ensure the Gradle installation's `bin` directory is on `PATH`.

Expected application output: `WendyHelloWorld() = 42`.

The example is a separate Gradle application. Its dependency is the local `distributions/kotlin/build/libs/wendy-core-<numeric-version>-dev.jar`, which contains the JNI bridge and Go shared library. It does not use a project dependency, Maven Local, or a published Core artifact. Both builds use the installed Gradle 9.8.1; no wrapper is bundled.
