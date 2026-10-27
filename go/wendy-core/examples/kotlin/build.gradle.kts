plugins {
    kotlin("jvm") version "2.4.21"
    application
}

repositories { mavenCentral() }
kotlin { jvmToolchain(21) }

val coreVersion = providers.environmentVariable("WENDY_CORE_VERSION")
    .orElse(file("../../VERSION").readText().trim().substringBefore('-') + "-dev").get()
dependencies {
    implementation(files("../../distributions/kotlin/build/libs/wendy-core-$coreVersion.jar"))
}

application { mainClass.set("HelloWorldKt") }
