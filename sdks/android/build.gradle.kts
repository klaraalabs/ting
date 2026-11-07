plugins {
    id("com.android.library") version "9.2.1"
    id("org.jetbrains.kotlin.plugin.serialization") version "2.2.10"
    `maven-publish`
}

group = "com.klaraalabs"
version = "0.1.0-SNAPSHOT"

android {
    namespace = "com.klaraalabs.ting"
    compileSdk = 36
    defaultConfig { minSdk = 24 }
    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
    testOptions.unitTests.all {
        it.systemProperty("ting.contracts", rootProject.file("../../tests/contracts").absolutePath)
    }
    publishing { singleVariant("release") { withSourcesJar() } }
}
kotlin { compilerOptions { jvmTarget.set(org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17) } }

dependencies {
    api("org.jetbrains.kotlinx:kotlinx-coroutines-core:1.10.2")
    implementation("org.jetbrains.kotlinx:kotlinx-coroutines-android:1.10.2")
    implementation("org.jetbrains.kotlinx:kotlinx-serialization-json:1.9.0")
    implementation("com.squareup.okhttp3:okhttp:4.12.0")
    api("com.google.firebase:firebase-messaging:25.0.1")
    api("androidx.activity:activity:1.11.0")
    implementation("androidx.core:core:1.17.0")
    testImplementation("junit:junit:4.13.2")
    testImplementation("org.jetbrains.kotlinx:kotlinx-coroutines-test:1.10.2")
    testImplementation("com.squareup.okhttp3:mockwebserver:4.12.0")
}

afterEvaluate {
    publishing.publications.create<MavenPublication>("release") {
        from(components["release"])
        artifactId = "ting-android"
    }
}
