package com.m0n5ter.monitor.data.network

import kotlinx.serialization.json.Json
import okhttp3.Credentials
import okhttp3.MediaType.Companion.toMediaType
import okhttp3.OkHttpClient
import okhttp3.logging.HttpLoggingInterceptor
import retrofit2.Retrofit
import retrofit2.converter.kotlinx.serialization.asConverterFactory
import java.time.Duration
import java.util.concurrent.ConcurrentHashMap

private val json = Json {
    ignoreUnknownKeys = true
    isLenient = true
}

private val httpClient: OkHttpClient by lazy {
    OkHttpClient.Builder()
        .connectTimeout(Duration.ofSeconds(8))
        .readTimeout(Duration.ofSeconds(15))
        .writeTimeout(Duration.ofSeconds(15))
        .addInterceptor(HttpLoggingInterceptor().apply { level = HttpLoggingInterceptor.Level.BASIC })
        .build()
}

/**
 * The shared client, sending the mesh's dashboard password as HTTP basic
 * authentication when there is one. The user name is not checked by the
 * server, so it is left empty. Derived with newBuilder() so every variant
 * shares one connection pool.
 */
fun authenticatedClient(password: String?): OkHttpClient {
    if (password.isNullOrEmpty()) return httpClient
    val header = Credentials.basic("", password, Charsets.UTF_8)
    return httpClient.newBuilder()
        .addInterceptor { chain ->
            chain.proceed(chain.request().newBuilder().header("Authorization", header).build())
        }
        .build()
}

/** One ApiService per base URL and password, cached so switching servers doesn't rebuild Retrofit constantly. */
object ApiClientFactory {
    private val cache = ConcurrentHashMap<Pair<String, String?>, ApiService>()

    fun forBaseUrl(baseUrl: String, password: String?): ApiService = cache.getOrPut(baseUrl to password) {
        val normalized = if (baseUrl.endsWith("/")) baseUrl else "$baseUrl/"
        Retrofit.Builder()
            .baseUrl(normalized)
            .client(authenticatedClient(password))
            .addConverterFactory(json.asConverterFactory("application/json".toMediaType()))
            .build()
            .create(ApiService::class.java)
    }
}
