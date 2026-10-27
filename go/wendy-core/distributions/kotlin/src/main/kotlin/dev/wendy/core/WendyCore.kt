package dev.wendy.core

import java.nio.file.Files

private object NativeBridge {
    private val directory = Files.createTempDirectory("wendy-core-").toFile().apply { deleteOnExit() }
    private val target: String
    private val coreName: String
    private val bridgeName: String
    private val handle: Long

    init {
        val os = System.getProperty("os.name").lowercase()
        val platform = when {
            os.contains("mac") -> "darwin"
            os.contains("linux") -> "linux"
            os.contains("windows") -> "win32"
            else -> error("Unsupported Wendy Core platform: $os")
        }
        val architecture = when (System.getProperty("os.arch")) {
            "aarch64", "arm64" -> "arm64"
            "amd64", "x86_64" -> "x64"
            else -> error("Unsupported Wendy Core architecture")
        }
        target = "$platform-$architecture"
        coreName = when (platform) {
            "darwin" -> "libWendyCore.dylib"
            "linux" -> "libWendyCore.so"
            else -> "WendyCore.dll"
        }
        bridgeName = when (platform) {
            "darwin" -> "libwendy_core_jni.dylib"
            "linux" -> "libwendy_core_jni.so"
            else -> "wendy_core_jni.dll"
        }
        val core = extract(coreName)
        System.load(extract(bridgeName).absolutePath)
        handle = open(core.absolutePath.toByteArray(Charsets.UTF_8))
    }

    private fun extract(name: String): java.io.File {
        val file = directory.resolve(name)
        val stream = NativeBridge::class.java.getResourceAsStream("/native/$target/$name")
            ?: error("Wendy Core is not packaged for $target")
        stream.use { input -> file.outputStream().use { output -> input.copyTo(output) } }
        file.deleteOnExit()
        return file
    }

    private external fun open(path: ByteArray): Long
    private external fun hello(handle: Long): Int
    private external fun version(handle: Long): String
    fun coreVersion(): String = version(handle)
    fun call(): Int = hello(handle)
}

/** Returns 42 from the shared Go engine. */
fun WendyHelloWorld(): Int = NativeBridge.call()

/** Returns the effective version of the loaded Wendy Core engine. */
fun WendyCoreVersion(): String = NativeBridge.coreVersion()
