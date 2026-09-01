package com.m0n5ter.monitor.data.network

import kotlinx.serialization.json.Json
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

/** One ApiService per base URL, cached so switching servers doesn't rebuild Retrofit constantly. */
object ApiClientFactory {
    private val cache = ConcurrentHashMap<String, ApiService>()

    fun forBaseUrl(baseUrl: String): ApiService = cache.getOrPut(baseUrl) {
        val normalized = if (baseUrl.endsWith("/")) baseUrl else "$baseUrl/"
        Retrofit.Builder()
            .baseUrl(normalized)
            .client(httpClient)
            .addConverterFactory(json.asConverterFactory("application/json".toMediaType()))
            .build()
            .create(ApiService::class.java)
    }
}
