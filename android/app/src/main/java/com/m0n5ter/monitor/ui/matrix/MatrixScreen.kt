package com.m0n5ter.monitor.ui.matrix

import androidx.compose.foundation.background
import androidx.compose.foundation.horizontalScroll
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import com.m0n5ter.monitor.data.model.ServerView
import com.m0n5ter.monitor.ui.UiState
import com.m0n5ter.monitor.ui.components.colorForPercent

private val NameColumnWidth = 108.dp
private val CellWidth = 84.dp
private val RowHeight = 56.dp

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun MatrixScreen(viewModel: MatrixViewModel) {
    val state by viewModel.state.collectAsState()

    when (val s = state) {
        is UiState.Loading -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
            CircularProgressIndicator()
        }

        is UiState.Error -> Box(Modifier.fillMaxSize().padding(24.dp), contentAlignment = Alignment.Center) {
            Text(s.message, textAlign = TextAlign.Center, style = MaterialTheme.typography.bodyLarge)
        }

        is UiState.Success -> PullToRefreshBox(
            isRefreshing = s.refreshing,
            onRefresh = viewModel::refresh,
            modifier = Modifier.fillMaxSize(),
        ) {
            if (s.data.servers.size < 2) {
                Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                    Text(
                        "Add a peer to see reachability between nodes.",
                        style = MaterialTheme.typography.bodyLarge,
                        textAlign = TextAlign.Center,
                        modifier = Modifier.padding(24.dp),
                    )
                }
            } else {
                MatrixGrid(s.data)
            }
        }
    }
}

@Composable
private fun MatrixGrid(data: MatrixUi) {
    val hScroll = rememberScrollState()

    Column(Modifier.fillMaxSize()) {
        val window = windowPhrase(data.windowSeconds)
        Text(
            "Rows are the observer, columns are what they see." +
                if (window.isNotEmpty()) " Last $window." else "",
            style = MaterialTheme.typography.bodyMedium,
            color = MaterialTheme.colorScheme.onSurfaceVariant,
            modifier = Modifier.padding(16.dp),
        )

        // Header row: corner cell frozen, column labels scroll with the body.
        Row(Modifier.fillMaxWidth()) {
            Box(Modifier.width(NameColumnWidth).height(RowHeight), contentAlignment = Alignment.CenterStart) {}
            Row(Modifier.horizontalScroll(hScroll)) {
                data.servers.forEach { to ->
                    Box(
                        Modifier.width(CellWidth).height(RowHeight),
                        contentAlignment = Alignment.Center,
                    ) {
                        Text(
                            to.name,
                            style = MaterialTheme.typography.labelSmall,
                            textAlign = TextAlign.Center,
                            maxLines = 2,
                        )
                    }
                }
            }
        }
        HorizontalDivider()

        LazyColumn(Modifier.fillMaxSize()) {
            items(data.servers, key = { it.id }) { from ->
                Row(Modifier.fillMaxWidth()) {
                    Box(
                        Modifier.width(NameColumnWidth).height(RowHeight).padding(start = 8.dp),
                        contentAlignment = Alignment.CenterStart,
                    ) {
                        Text(from.name, style = MaterialTheme.typography.labelSmall, maxLines = 2)
                    }
                    Row(Modifier.horizontalScroll(hScroll)) {
                        data.servers.forEach { to ->
                            MatrixCell(from, to, data)
                        }
                    }
                }
                HorizontalDivider(color = MaterialTheme.colorScheme.surfaceVariant)
            }
        }
    }
}

@Composable
private fun MatrixCell(from: ServerView, to: ServerView, data: MatrixUi) {
    Box(
        Modifier.width(CellWidth).height(RowHeight),
        contentAlignment = Alignment.Center,
    ) {
        if (from.id == to.id) {
            Text("—", color = MaterialTheme.colorScheme.onSurfaceVariant)
            return@Box
        }
        val entry = data.entries[from.id to to.id]
        if (entry == null) {
            Text("no data", style = MaterialTheme.typography.labelSmall, color = MaterialTheme.colorScheme.onSurfaceVariant)
        } else {
            val color = when {
                !entry.isAvailable -> MaterialTheme.colorScheme.error
                else -> colorForPercent(100.0 - entry.availabilityPercent, warn = 5.0, danger = 20.0)
            }
            Box(
                Modifier
                    .width(64.dp)
                    .height(32.dp)
                    .background(color.copy(alpha = 0.18f), RoundedCornerShape(6.dp)),
                contentAlignment = Alignment.Center,
            ) {
                Text(
                    "${entry.availabilityPercent.toInt()}%",
                    style = MaterialTheme.typography.labelSmall,
                    color = color,
                )
            }
        }
    }
}

// Same wording as the dashboard's windowPhrase, so the app and the page
// describe the window the server sends in the same words.
private fun windowPhrase(secs: Int): String {
    if (secs <= 0) return ""
    if (secs < 90) return "$secs seconds"
    val min = Math.round(secs / 60.0).toInt()
    if (min < 90) return "$min minutes"
    val h = Math.round(min / 60.0).toInt()
    return if (h == 1) "hour" else "$h hours"
}
