# Wendy Core for Kotlin/JVM

The Maven artifact `dev.wendy:wendy-core` provides thin JNI access to the same native Core release.

```kotlin
import dev.wendy.core.WendyHelloWorld
check(WendyHelloWorld() == 42)
```

The release jar contains every supported native target plus a minimal JNI loader. Local builds require JDK 21+ and Gradle 9.8.1 installed on `PATH`; the Gradle build selects the Kotlin compiler. Consumers need a compiler compatible with Kotlin 2.4 metadata. Libraries are extracted to a private temporary directory and loaded for the process lifetime; runtime notices accompany them.

Kotlin/JVM distribution support is not an Android platform-support claim. Android/Native/WASM variants require their own verified target integration. This is not the future ergonomic Wendy Kotlin SDK.

From `go/wendy-core/`, run `make kotlin` to build the JNI bridge and test the
distribution. To build the jar after the native bridge is built, run
`gradle -p distributions/kotlin jar --no-daemon`. No Gradle wrapper is bundled;
CI installs Gradle 9.8.1 with its pinned setup-gradle action.
