plugins {
    kotlin("jvm") version "2.4.21"
    `maven-publish`
}

group = "dev.wendy"
version = providers.environmentVariable("WENDY_CORE_VERSION")
    .orElse(providers.gradleProperty("coreVersion").map { "${it.substringBefore('-')}-dev" }).get()
repositories { mavenCentral() }
kotlin { jvmToolchain(21) }
dependencies { testImplementation(kotlin("test")) }
tasks.test { useJUnitPlatform() }
java { withSourcesJar(); withJavadocJar() }
tasks.withType<Jar>().configureEach {
    from(listOf("../../LICENSE", "../../NOTICE")) { into("META-INF") }
}

publishing {
    publications {
        create<MavenPublication>("core") {
            from(components["java"])
            pom {
                name.set("Wendy Core")
                description.set("Thin Kotlin/JVM access to the shared Wendy Core engine")
                url.set("https://github.com/wendylabsinc/WendyOS/tree/main/go/wendy-core")
                licenses {
                    license {
                        name.set("Apache License, Version 2.0")
                        url.set("https://www.apache.org/licenses/LICENSE-2.0")
                        distribution.set("repo")
                    }
                }
            }
        }
    }
    repositories {
        maven {
            name = "release"
            url = uri(providers.environmentVariable("MAVEN_PUBLISH_URL").orElse("https://maven.pkg.github.com/wendylabsinc/wendy-core").get())
            credentials {
                username = providers.environmentVariable("MAVEN_USERNAME").orNull
                password = providers.environmentVariable("MAVEN_PASSWORD").orNull
            }
        }
    }
}
