package com.m0n5ter.monitor.ui.connection

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.m0n5ter.monitor.data.model.HealthResponse
import com.m0n5ter.monitor.data.settings.ConnectionStore
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.SharingStarted
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.stateIn
import kotlinx.coroutines.launch
import kotlinx.serialization.json.Json
import okhttp3.MediaType.Companion.toMediaType
import retrofit2.HttpException
import retrofit2.Retrofit
import retrofit2.converter.kotlinx.serialization.asConverterFactory
import retrofit2.http.GET

sealed interface ConnectResult {
    data object Idle : ConnectResult
    data object Connecting : ConnectResult
    data class Failed(val message: String) : ConnectResult
}

class ConnectionViewModel(private val store: ConnectionStore) : ViewModel() {

    val connectionState: StateFlow<ConnectionStore.State> = store.state.stateIn(
        viewModelScope, SharingStarted.WhileSubscribed(5_000), ConnectionStore.State(emptyList(), null),
    )

    private val _connectResult = MutableStateFlow<ConnectResult>(ConnectResult.Idle)
    val connectResult: StateFlow<ConnectResult> = _connectResult

    /** Verifies the address answers /api/health before saving it, so a typo fails fast. */
    fun connect(rawUrl: String) {
        val url = ConnectionStore.normalize(rawUrl)
        if (url.isBlank()) return
        viewModelScope.launch {
            _connectResult.value = ConnectResult.Connecting
            try {
                healthCheck(url)
                store.setActive(url)
                _connectResult.value = ConnectResult.Idle
            } catch (e: Exception) {
                _connectResult.value = ConnectResult.Failed(describe(e))
            }
        }
    }

    fun selectSaved(url: String) {
        viewModelScope.launch { store.setActive(url) }
    }

    fun forget(url: String) {
        viewModelScope.launch { store.forget(url) }
    }

    private suspend fun healthCheck(url: String): HealthResponse {
        val json = Json { ignoreUnknownKeys = true; isLenient = true }
        val normalized = if (url.endsWith("/")) url else "$url/"
        val retrofit = Retrofit.Builder()
            .baseUrl(normalized)
            .addConverterFactory(json.asConverterFactory("application/json".toMediaType()))
            .build()
        return retrofit.create(HealthApi::class.java).health()
    }

    private fun describe(e: Exception): String = when (e) {
        is HttpException -> "Server responded with an error (${e.code()})."
        is java.net.ConnectException -> "Can't reach that address. Check the host, port and that the server is running."
        is java.net.UnknownHostException -> "Unknown host."
        is java.net.SocketTimeoutException -> "Timed out waiting for a response."
        else -> e.message ?: "Could not connect."
    }
}

private interface HealthApi {
    @GET("api/health")
    suspend fun health(): HealthResponse
}
