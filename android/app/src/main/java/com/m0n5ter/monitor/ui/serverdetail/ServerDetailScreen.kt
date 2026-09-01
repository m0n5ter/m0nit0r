package com.m0n5ter.monitor.ui.serverdetail

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.FilterChip
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.pulltorefresh.PullToRefreshBox
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import com.m0n5ter.monitor.data.model.Metric
import com.m0n5ter.monitor.ui.UiState
import com.m0n5ter.monitor.ui.components.LineChart
import com.m0n5ter.monitor.ui.components.colorForPercent
import com.m0n5ter.monitor.util.parseServerTime
import com.m0n5ter.monitor.util.toLocalDateTimeLabel
import com.m0n5ter.monitor.util.toLocalTimeLabel

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun ServerDetailScreen(viewModel: ServerDetailViewModel) {
    val state by viewModel.state.collectAsState()
    val window by viewModel.window.collectAsState()

    Column(Modifier.fillMaxSize()) {
        Row(
            Modifier
                .fillMaxWidth()
                .padding(horizontal = 16.dp, vertical = 12.dp),
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            TimeWindow.entries.forEach { w ->
                FilterChip(
                    selected = w == window,
                    onClick = { viewModel.selectWindow(w) },
                    label = { Text(w.label) },
                )
            }
        }

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
                if (s.data.isEmpty()) {
                    Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                        Text("No metrics in this window.", style = MaterialTheme.typography.bodyLarge)
                    }
                } else {
                    MetricsBody(s.data)
                }
            }
        }
    }
}

@Composable
private fun MetricsBody(metrics: List<Metric>) {
    val latest = metrics.last()
    val hasTemp = metrics.any { it.cpuTempC != null }

    LazyColumn(
        modifier = Modifier.fillMaxSize(),
        contentPadding = PaddingValues(16.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        item {
            ChartCard(
                title = "CPU",
                valueLabel = "${"%.0f".format(latest.cpuPercent)}%",
                values = metrics.map { it.cpuPercent.toFloat() },
                color = colorForPercent(latest.cpuPercent),
                firstTimestamp = metrics.first().timestamp,
                lastTimestamp = metrics.last().timestamp,
            )
        }
        item {
            ChartCard(
                title = "Memory",
                valueLabel = "${"%.0f".format(latest.memoryPercent)}% · ${"%.1f".format(latest.memoryUsedMb / 1024)} / ${"%.1f".format(latest.memoryTotalMb / 1024)} GB",
                values = metrics.map { it.memoryPercent.toFloat() },
                color = colorForPercent(latest.memoryPercent),
                firstTimestamp = metrics.first().timestamp,
                lastTimestamp = metrics.last().timestamp,
            )
        }
        if (hasTemp) {
            item {
                val tempValues = metrics.map { (it.cpuTempC ?: 0.0).toFloat() }
                ChartCard(
                    title = "CPU temperature",
                    valueLabel = latest.cpuTempC?.let { "${"%.0f".format(it)}°C" } ?: "—",
                    values = tempValues,
                    color = colorForPercent(latest.cpuTempC ?: 0.0, warn = 70.0, danger = 85.0),
                    minValue = 0f,
                    maxValue = 100f,
                    firstTimestamp = metrics.first().timestamp,
                    lastTimestamp = metrics.last().timestamp,
                )
            }
        }
        if (latest.disks.isNotEmpty()) {
            item {
                Text("Disks", style = MaterialTheme.typography.titleMedium)
            }
            items(latest.disks, key = { it.name }) { disk ->
                Card(Modifier.fillMaxWidth()) {
                    Column(Modifier.padding(16.dp)) {
                        Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                            Text(disk.name, style = MaterialTheme.typography.titleMedium)
                            Text(
                                "${"%.0f".format(disk.usagePercent)}%",
                                style = MaterialTheme.typography.titleMedium,
                                color = colorForPercent(disk.usagePercent),
                            )
                        }
                        Spacer(Modifier.size(4.dp))
                        Text(
                            "${"%.0f".format(disk.usedGb)} / ${"%.0f".format(disk.totalGb)} GB used" +
                                (disk.tempC?.let { " · ${"%.0f".format(it)}°C" } ?: ""),
                            style = MaterialTheme.typography.bodyMedium,
                            color = MaterialTheme.colorScheme.onSurfaceVariant,
                        )
                    }
                }
            }
        }
    }
}

@Composable
private fun ChartCard(
    title: String,
    valueLabel: String,
    values: List<Float>,
    color: Color,
    firstTimestamp: String,
    lastTimestamp: String,
    minValue: Float = 0f,
    maxValue: Float = 100f,
) {
    Card(Modifier.fillMaxWidth()) {
        Column(Modifier.padding(16.dp)) {
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                Text(title, style = MaterialTheme.typography.titleMedium)
                Text(valueLabel, style = MaterialTheme.typography.titleMedium, color = color)
            }
            Spacer(Modifier.size(8.dp))
            LineChart(values = values, minValue = minValue, maxValue = maxValue, lineColor = color)
            Row(Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.SpaceBetween) {
                Text(
                    parseServerTime(firstTimestamp).toLocalTimeLabel(),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
                Text(
                    parseServerTime(lastTimestamp).toLocalDateTimeLabel(),
                    style = MaterialTheme.typography.labelSmall,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                )
            }
        }
    }
}
