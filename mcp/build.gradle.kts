plugins {
    alias(libs.plugins.shadow)
    application
}

dependencies {
    implementation(project(":core"))
    implementation(libs.kotlin.stdlib)
    implementation(libs.coroutines.core)
    implementation(libs.mcp.kotlin)
}

application {
    mainClass.set("cn.fangxiang.easypublisher.mcp.MainKt")
    applicationName = "easy-publisher-mcp"
}

tasks.shadowJar {
    archiveBaseName.set("easy-publisher-mcp")
    archiveClassifier.set("")
    manifest {
        attributes("Main-Class" to "cn.fangxiang.easypublisher.mcp.MainKt")
    }
    mergeServiceFiles()
}
