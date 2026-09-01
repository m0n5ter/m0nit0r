package com.m0n5ter.monitor.ui.servers

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.m0n5ter.monitor.data.model.ServerView
import com.m0n5ter.monitor.data.repository.MonitorRepository
import com.m0n5ter.monitor.ui.UiState
import com.m0n5ter.monitor.ui.toUserMessage
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

private const val POLL_INTERVAL_MS = 5_000L

class ServersViewModel(private val repository: MonitorRepository) : ViewModel() {

    private val _state = MutableStateFlow<UiState<List<ServerView>>>(UiState.Loading)
    val state: StateFlow<UiState<List<ServerView>>> = _state

    init {
        startPolling()
    }

    private fun startPolling() {
        viewModelScope.launch {
            while (true) {
                load(isBackground = _state.value is UiState.Success)
                delay(POLL_INTERVAL_MS)
            }
        }
    }

    fun refresh() {
        viewModelScope.launch { load(isBackground = false, showSpinner = true) }
    }

    private suspend fun load(isBackground: Boolean, showSpinner: Boolean = false) {
        if (showSpinner) {
            (_state.value as? UiState.Success)?.let { current ->
                _state.update { UiState.Success(current.data, refreshing = true) }
            }
        }
        try {
            val servers = repository.servers().sortedWith(
                compareByDescending<ServerView> { it.isSelf }.thenBy { it.name.lowercase() },
            )
            _state.value = UiState.Success(servers)
        } catch (e: Exception) {
            if (!isBackground) {
                _state.value = UiState.Error(e.toUserMessage())
            }
            // A background poll failing silently keeps the last-known list on
            // screen instead of replacing it with an error every 5 seconds.
        }
    }
}
