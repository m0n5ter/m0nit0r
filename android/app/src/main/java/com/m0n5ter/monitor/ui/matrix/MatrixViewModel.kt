package com.m0n5ter.monitor.ui.matrix

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.m0n5ter.monitor.data.model.MatrixEntry
import com.m0n5ter.monitor.data.model.ServerView
import com.m0n5ter.monitor.data.repository.MonitorRepository
import com.m0n5ter.monitor.ui.UiState
import com.m0n5ter.monitor.ui.toUserMessage
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class MatrixUi(
    val servers: List<ServerView>,
    val entries: Map<Pair<String, String>, MatrixEntry>,
    val windowSeconds: Int,
)

private const val POLL_INTERVAL_MS = 10_000L

class MatrixViewModel(private val repository: MonitorRepository) : ViewModel() {

    private val _state = MutableStateFlow<UiState<MatrixUi>>(UiState.Loading)
    val state: StateFlow<UiState<MatrixUi>> = _state

    init {
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
            val servers = repository.servers()
            val matrix = repository.availabilityMatrix()
            val entries = matrix.edges.associateBy { it.fromServerId to it.toServerId }
            _state.value = UiState.Success(MatrixUi(servers, entries, matrix.windowSeconds))
        } catch (e: Exception) {
            if (!isBackground) _state.value = UiState.Error(e.toUserMessage())
        }
    }
}
