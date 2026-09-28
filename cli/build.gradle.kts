plugins {
    alias(libs.plugins.shadow)
    application
}

dependencies {
    implementation(project(":core"))
    implementation(libs.kotlin.stdlib)
    implementation(libs.coroutines.core)
    implementation(libs.clikt)
    implementation(libs.moshi)
}

application {
    mainClass.set("cn.fangxiang.easypublisher.cli.MainKt")
    applicationName = "easy-publisher"
}

tasks.shadowJar {
    archiveBaseName.set("easy-publisher")
    archiveClassifier.set("")
    manifest {
        attributes("Main-Class" to "cn.fangxiang.easypublisher.cli.MainKt")
    }
    mergeServiceFiles()
}
