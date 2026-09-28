plugins {
    alias(libs.plugins.kotlin.jvm) apply false
}

val appVersion = "1.0.0"

allprojects {
    group = "cn.fangxiang"
    version = appVersion
}

subprojects {
    apply(plugin = "org.jetbrains.kotlin.jvm")

    repositories {
        mavenCentral()
    }

    extensions.configure<org.jetbrains.kotlin.gradle.dsl.KotlinJvmProjectExtension> {
        jvmToolchain(17)
        compilerOptions {
            freeCompilerArgs.add("-Xjsr305=strict")
        }
    }

    dependencies {
        add("testImplementation", rootProject.libs.kotlin.test)
        add("testImplementation", rootProject.libs.junit.jupiter)
        add("testImplementation", rootProject.libs.coroutines.test)
        add("testRuntimeOnly", rootProject.libs.junit.platform.launcher)
    }

    tasks.withType<Test>().configureEach {
        useJUnitPlatform()
        // Gradle 默认不把 -D 转发给测试 JVM，黄金向量的重新生成开关需要显式传递
        systemProperty("golden.update", System.getProperty("golden.update") ?: "false")
        testLogging {
            events("passed", "skipped", "failed")
        }
    }
}
