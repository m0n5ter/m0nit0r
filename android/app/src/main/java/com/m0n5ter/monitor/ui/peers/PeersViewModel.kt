package com.m0n5ter.monitor.ui.peers

import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewModelScope
import com.m0n5ter.monitor.data.model.PeerView
import com.m0n5ter.monitor.data.repository.MonitorRepository
import com.m0n5ter.monitor.ui.UiState
import com.m0n5ter.monitor.ui.toUserMessage
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.launch

class PeersViewModel(private val repository: MonitorRepository) : ViewModel() {

    private val _state = MutableStateFlow<UiState<List<PeerView>>>(UiState.Loading)
    val state: StateFlow<UiState<List<PeerView>>> = _state

    private val _actionError = MutableStateFlow<String?>(null)
    val actionError: StateFlow<String?> = _actionError

    private val _adding = MutableStateFlow(false)
    val adding: StateFlow<Boolean> = _adding

    init {
        refresh()
    }

    fun refresh() {
        viewModelScope.launch {
            try {
                _state.value = UiState.Success(repository.peers())
            } catch (e: Exception) {
                _state.value = UiState.Error(e.toUserMessage())
            }
        }
    }

    fun addPeer(url: String) {
        if (url.isBlank()) return
        viewModelScope.launch {
            _adding.value = true
            _actionError.value = null
            try {
                repository.addPeer(url.trim())
                refresh()
            } catch (e: Exception) {
                _actionError.value = e.toUserMessage()
            } finally {
                _adding.value = false
            }
        }
    }

    fun removePeer(id: String) {
        viewModelScope.launch {
            try {
                repository.removePeer(id)
                refresh()
            } catch (e: Exception) {
                _actionError.value = e.toUserMessage()
            }
        }
    }

    fun dismissError() {
        _actionError.value = null
    }
}
