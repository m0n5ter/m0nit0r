package com.m0n5ter.monitor.ui

/** Shared loading/success/error shape for the screens that poll one endpoint. */
sealed interface UiState<out T> {
    data object Loading : UiState<Nothing>
    data class Success<T>(val data: T, val refreshing: Boolean = false) : UiState<T>
    data class Error(val message: String) : UiState<Nothing>
}

fun Throwable.toUserMessage(): String = when (this) {
    is java.net.ConnectException -> "Can't reach the server. Check the address and that it's running."
    is java.net.SocketTimeoutException -> "The server didn't respond in time."
    is java.net.UnknownHostException -> "Unknown host. Check the address."
    else -> message ?: "Something went wrong."
}
