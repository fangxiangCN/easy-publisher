dependencies {
    implementation(libs.kotlin.stdlib)
    implementation(libs.coroutines.core)

    implementation(libs.okhttp)
    implementation(libs.retrofit)
    implementation(libs.retrofit.moshi)
    implementation(libs.moshi)
    implementation(libs.moshi.kotlin)

    // 小米渠道的 RSA/NONE/PKCS1Padding
    implementation(libs.bouncycastle)

    implementation(libs.apk.parser)

    testImplementation(libs.mockwebserver)
}
