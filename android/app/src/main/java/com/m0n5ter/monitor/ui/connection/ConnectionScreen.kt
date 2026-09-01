package com.m0n5ter.monitor.ui.connection

import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.WindowInsets
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.safeDrawing
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Check
import androidx.compose.material.icons.filled.Close
import androidx.compose.material.icons.filled.Dns
import androidx.compose.material3.Button
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.HorizontalDivider
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.unit.dp

@Composable
fun ConnectionScreen(
    viewModel: ConnectionViewModel,
    showCancel: Boolean,
    onConnected: () -> Unit,
    onCancel: () -> Unit = {},
) {
    val connectionState by viewModel.connectionState.collectAsState()
    val connectResult by viewModel.connectResult.collectAsState()
    var input by remember { mutableStateOf("") }

    // A successful connect() saves the URL, which flips ConnectionStore.State
    // .activeUrl; MonitorApp watches that and swaps this screen out on its
    // own, so there is nothing to do here beyond the explicit onConnected()
    // call below for tapping a saved server directly.

    // This screen is shown both as the first-run root - with no Scaffold above
    // it - and from the Settings tab, so it carries its own background and
    // window insets instead of trusting the caller to supply them. safeDrawing
    // covers the status bar, the navigation bar and the IME; the last one
    // matters because this is the only screen with a text field.
    Scaffold(contentWindowInsets = WindowInsets.safeDrawing) { insets ->
        Column(
            Modifier
                .fillMaxSize()
                .padding(insets)
                .verticalScroll(rememberScrollState())
                .padding(24.dp),
        ) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Icon(Icons.Filled.Dns, contentDescription = null, tint = MaterialTheme.colorScheme.primary, modifier = Modifier.size(28.dp))
                Spacer(Modifier.size(12.dp))
                Text("Connect to m0nit0r", style = MaterialTheme.typography.titleLarge)
            }
            Spacer(Modifier.size(8.dp))
            Text(
                "Point this at any one node in the mesh - it holds the full picture for all of them.",
                style = MaterialTheme.typography.bodyMedium,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
            )
            Spacer(Modifier.size(24.dp))

            OutlinedTextField(
                value = input,
                onValueChange = { input = it },
                label = { Text("Server address") },
                placeholder = { Text("192.168.1.10:5001") },
                singleLine = true,
                modifier = Modifier.fillMaxWidth(),
            )
            Spacer(Modifier.size(12.dp))
            Button(
                onClick = { viewModel.connect(input) },
                enabled = input.isNotBlank() && connectResult != ConnectResult.Connecting,
                modifier = Modifier.fillMaxWidth(),
            ) {
                if (connectResult == ConnectResult.Connecting) {
                    CircularProgressIndicator(modifier = Modifier.size(18.dp), color = MaterialTheme.colorScheme.onPrimary)
                } else {
                    Text("Connect")
                }
            }

            (connectResult as? ConnectResult.Failed)?.let { failed ->
                Spacer(Modifier.size(8.dp))
                Text(failed.message, color = MaterialTheme.colorScheme.error, style = MaterialTheme.typography.bodyMedium)
            }

            if (connectionState.savedUrls.isNotEmpty()) {
                Spacer(Modifier.size(28.dp))
                Text("Saved servers", style = MaterialTheme.typography.titleMedium)
                Spacer(Modifier.size(8.dp))
                // A plain Column, not LazyColumn: this list is a handful of saved
                // addresses at most, and the screen already scrolls as a whole -
                // a nested scrollable here would fight that for vertical space.
                connectionState.savedUrls.forEach { url ->
                    Card(
                        modifier = Modifier.fillMaxWidth().padding(vertical = 4.dp),
                        onClick = {
                            viewModel.selectSaved(url)
                            onConnected()
                        },
                    ) {
                        Row(
                            Modifier.fillMaxWidth().padding(12.dp),
                            verticalAlignment = Alignment.CenterVertically,
                        ) {
                            if (url == connectionState.activeUrl) {
                                Icon(Icons.Filled.Check, contentDescription = "Active", tint = MaterialTheme.colorScheme.primary)
                                Spacer(Modifier.size(8.dp))
                            }
                            Text(url, modifier = Modifier.weight(1f), style = MaterialTheme.typography.bodyLarge)
                            IconButton(onClick = { viewModel.forget(url) }) {
                                Icon(Icons.Filled.Close, contentDescription = "Forget")
                            }
                        }
                    }
                }
            }

            if (showCancel) {
                Spacer(Modifier.size(16.dp))
                HorizontalDivider()
                Spacer(Modifier.size(16.dp))
                TextButton(onClick = onCancel, modifier = Modifier.fillMaxWidth()) {
                    Text("Cancel")
                }
            }
        }
    }
}
