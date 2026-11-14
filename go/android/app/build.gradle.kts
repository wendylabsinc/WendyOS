plugins {
    id("com.android.application")
    id("org.jetbrains.kotlin.android")
    id("org.jetbrains.kotlin.plugin.compose")
}
android {
    namespace = "dev.wendy.android"
    compileSdk = 36
    defaultConfig {
        applicationId = "dev.wendy.android"
        minSdk = 29
        targetSdk = 36
        versionCode = 1
        versionName = "0.1.0"
        testInstrumentationRunner = "androidx.test.runner.AndroidJUnitRunner"
    }
    buildFeatures { compose = true; buildConfig = true }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    packaging { jniLibs.useLegacyPackaging = false }
    buildTypes { release { isMinifyEnabled = false } }
}
val buildGo by tasks.registering(Exec::class) {
    workingDir(rootProject.projectDir)
    commandLine("bash", "scripts/build-go.sh")
    inputs.files(fileTree(rootProject.file("../..")) {
        include("go/**/*.go", "go/**/*.c", "go/**/*.h", "go/**/*.m", "go.mod", "go.sum", "go/internal/cli/assets/**", "go/macos/backend/skills/**")
        exclude("go/android/app/**", "go/macos/Build/**")
    })
    inputs.file(rootProject.file("scripts/build-go.sh"))
    inputs.property("abis", providers.environmentVariable("WENDY_ANDROID_ABIS").orElse("arm64-v8a,x86_64"))
    outputs.dir("src/main/jniLibs")
}
tasks.named("preBuild").configure { dependsOn(buildGo) }
dependencies {
    implementation(platform("androidx.compose:compose-bom:2026.03.01"))
    implementation("androidx.activity:activity-compose:1.12.1")
    implementation("androidx.compose.material3:material3")
    implementation("androidx.compose.material:material-icons-extended")
    implementation("androidx.compose.ui:ui-tooling-preview")
    implementation("androidx.lifecycle:lifecycle-viewmodel-compose:2.10.0")
    implementation("androidx.lifecycle:lifecycle-runtime-compose:2.10.0")
    implementation("androidx.core:core-ktx:1.17.0")
    debugImplementation("androidx.compose.ui:ui-tooling")
    androidTestImplementation("androidx.test.ext:junit:1.3.0")
    androidTestImplementation("androidx.test:runner:1.7.0")
    androidTestImplementation("androidx.test:core:1.7.0")
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.json:json:20251224")
}

kotlin { compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17) } }
