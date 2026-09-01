package com.m0n5ter.monitor.ui.serverdetail

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.m0n5ter.monitor.data.model.Metric
import com.m0n5ter.monitor.data.repository.MonitorRepository
import com.m0n5ter.monitor.ui.UiState
import com.m0n5ter.monitor.ui.toUserMessage
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

/** Presets mirroring the dashboard's own window choices. */
enum class TimeWindow(val label: String, val hours: Int) {
    ONE_HOUR("1h", 1),
    SIX_HOURS("6h", 6),
    ONE_DAY("24h", 24),
    ONE_WEEK("7d", 168),
}

private const val POLL_INTERVAL_MS = 10_000L

class ServerDetailViewModel(
    private val repository: MonitorRepository,
    private val serverId: String,
) : ViewModel() {

    private val _window = MutableStateFlow(TimeWindow.ONE_DAY)
    val window: StateFlow<TimeWindow> = _window

    private val _state = MutableStateFlow<UiState<List<Metric>>>(UiState.Loading)
    val state: StateFlow<UiState<List<Metric>>> = _state

    init {
        startPolling()
    }

    fun selectWindow(w: TimeWindow) {
        if (_window.value == w) return
        _window.value = w
        _state.value = UiState.Loading
        viewModelScope.launch { load(isBackground = false) }
    }

    fun refresh() {
        viewModelScope.launch { load(isBackground = false, showSpinner = true) }
    }

    private fun startPolling() {
        viewModelScope.launch {
            while (true) {
                load(isBackground = _state.value is UiState.Success)
                delay(POLL_INTERVAL_MS)
            }
        }
    }

    private suspend fun load(isBackground: Boolean, showSpinner: Boolean = false) {
        if (showSpinner) {
            (_state.value as? UiState.Success)?.let { current ->
                _state.update { UiState.Success(current.data, refreshing = true) }
            }
        }
        try {
            val metrics = repository.serverMetrics(serverId, _window.value.hours)
            _state.value = UiState.Success(metrics)
        } catch (e: Exception) {
            if (!isBackground) _state.value = UiState.Error(e.toUserMessage())
        }
    }
}
