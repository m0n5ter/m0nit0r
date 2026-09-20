package com.m0n5ter.monitor.ui.peers

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.items
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.filled.Add
import androidx.compose.material.icons.filled.Delete
import androidx.compose.material3.AlertDialog
import androidx.compose.material3.Card
import androidx.compose.material3.CircularProgressIndicator
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.IconButton
import androidx.compose.material3.Icon
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
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
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import com.m0n5ter.monitor.data.model.PeerView
import com.m0n5ter.monitor.ui.UiState
import com.m0n5ter.monitor.util.parseServerTime
import com.m0n5ter.monitor.util.relativeToNow

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun PeersScreen(viewModel: PeersViewModel) {
    val state by viewModel.state.collectAsState()
    val adding by viewModel.adding.collectAsState()
    val actionError by viewModel.actionError.collectAsState()

    var newUrl by remember { mutableStateOf("") }
    var pendingRemoval by remember { mutableStateOf<PeerView?>(null) }

    Column(Modifier.fillMaxSize()) {
        Row(
            Modifier.fillMaxWidth().padding(16.dp),
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(8.dp),
        ) {
            OutlinedTextField(
                value = newUrl,
                onValueChange = { newUrl = it },
                label = { Text("Peer URL") },
                placeholder = { Text("http://host:5001") },
                singleLine = true,
                modifier = Modifier.weight(1f),
            )
            IconButton(
                onClick = {
                    viewModel.addPeer(newUrl)
                    newUrl = ""
                },
                enabled = !adding && newUrl.isNotBlank(),
            ) {
                if (adding) {
                    CircularProgressIndicator(modifier = Modifier.padding(4.dp))
                } else {
                    Icon(Icons.Filled.Add, contentDescription = "Add peer")
                }
            }
        }

        actionError?.let { message ->
            Text(
                message,
                color = MaterialTheme.colorScheme.error,
                style = MaterialTheme.typography.bodyMedium,
                modifier = Modifier.padding(horizontal = 16.dp, vertical = 4.dp),
            )
        }

        when (val s = state) {
            is UiState.Loading -> Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                CircularProgressIndicator()
            }

            is UiState.Error -> Box(Modifier.fillMaxSize().padding(24.dp), contentAlignment = Alignment.Center) {
                Text(s.message, textAlign = TextAlign.Center, style = MaterialTheme.typography.bodyLarge)
            }

            is UiState.Success -> if (s.data.isEmpty()) {
                Box(Modifier.fillMaxSize(), contentAlignment = Alignment.Center) {
                    Text(
                        "No peers configured. Add one above to build the mesh.",
                        style = MaterialTheme.typography.bodyLarge,
                        textAlign = TextAlign.Center,
                        modifier = Modifier.padding(24.dp),
                    )
                }
            } else {
                LazyColumn(
                    modifier = Modifier.fillMaxSize(),
                    contentPadding = PaddingValues(16.dp),
                    verticalArrangement = Arrangement.spacedBy(8.dp),
                ) {
                    items(s.data, key = { it.id }) { peer ->
                        Card(Modifier.fillMaxWidth()) {
                            Row(
                                Modifier.fillMaxWidth().padding(16.dp),
                                verticalAlignment = Alignment.CenterVertically,
                            ) {
                                Column(Modifier.weight(1f)) {
                                    Text(peer.name, style = MaterialTheme.typography.titleMedium)
                                    Text(
                                        peer.url,
                                        style = MaterialTheme.typography.bodyMedium,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                    Text(
                                        "${peer.location} · seen ${parseServerTime(peer.lastSeen).relativeToNow()}",
                                        style = MaterialTheme.typography.bodyMedium,
                                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                                    )
                                }
                                IconButton(onClick = { pendingRemoval = peer }) {
                                    Icon(Icons.Filled.Delete, contentDescription = "Remove peer", tint = MaterialTheme.colorScheme.error)
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    pendingRemoval?.let { peer ->
        AlertDialog(
            onDismissRequest = { pendingRemoval = null },
            title = { Text("Remove ${peer.name} from the mesh?") },
            text = {
                Text(
                    "Every node drops it, not just this one, and it stops being monitored anywhere. " +
                        "Collected history is kept until it ages out. Adding it back here lets it rejoin.",
                )
            },
            confirmButton = {
                TextButton(onClick = {
                    viewModel.removePeer(peer.id)
                    pendingRemoval = null
                }) { Text("Remove") }
            },
            dismissButton = {
                TextButton(onClick = { pendingRemoval = null }) { Text("Cancel") }
            },
        )
    }
}
