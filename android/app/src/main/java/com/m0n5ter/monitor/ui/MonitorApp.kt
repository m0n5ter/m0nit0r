package com.m0n5ter.monitor.ui

import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.padding
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.filled.ArrowBack
import androidx.compose.material.icons.filled.CompareArrows
import androidx.compose.material.icons.filled.Dns
import androidx.compose.material.icons.filled.Hub
import androidx.compose.material.icons.filled.Settings
import androidx.compose.material3.ExperimentalMaterial3Api
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.NavigationBar
import androidx.compose.material3.NavigationBarItem
import androidx.compose.material3.Scaffold
import androidx.compose.material3.Text
import androidx.compose.material3.TopAppBar
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.collectAsState
import androidx.compose.runtime.getValue
import androidx.compose.runtime.key
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.platform.LocalContext
import androidx.navigation.NavGraph.Companion.findStartDestination
import androidx.navigation.compose.NavHost
import androidx.navigation.compose.composable
import androidx.navigation.compose.currentBackStackEntryAsState
import androidx.navigation.compose.rememberNavController
import com.m0n5ter.monitor.data.model.ServerView
import com.m0n5ter.monitor.data.repository.MonitorRepository
import com.m0n5ter.monitor.data.settings.ConnectionStore
import com.m0n5ter.monitor.ui.connection.ConnectionScreen
import com.m0n5ter.monitor.ui.connection.ConnectionViewModel
import com.m0n5ter.monitor.ui.matrix.MatrixScreen
import com.m0n5ter.monitor.ui.matrix.MatrixViewModel
import com.m0n5ter.monitor.ui.navigation.Routes
import com.m0n5ter.monitor.ui.navigation.rememberViewModel
import com.m0n5ter.monitor.ui.peers.PeersScreen
import com.m0n5ter.monitor.ui.peers.PeersViewModel
import com.m0n5ter.monitor.ui.serverdetail.ServerDetailScreen
import com.m0n5ter.monitor.ui.serverdetail.ServerDetailViewModel
import com.m0n5ter.monitor.ui.servers.ServersScreen
import com.m0n5ter.monitor.ui.servers.ServersViewModel

private data class Tab(val route: String, val label: String, val icon: ImageVector)

private val tabs = listOf(
    Tab(Routes.Servers, "Servers", Icons.Filled.Dns),
    Tab(Routes.Matrix, "Matrix", Icons.Filled.CompareArrows),
    Tab(Routes.Peers, "Mesh", Icons.Filled.Hub),
    Tab(Routes.Settings, "Settings", Icons.Filled.Settings),
)

@Composable
fun MonitorApp() {
    val context = LocalContext.current
    val connectionStore = remember { ConnectionStore(context.applicationContext) }
    val connectionViewModel = rememberViewModel { ConnectionViewModel(connectionStore) }
    val connectionState by connectionViewModel.connectionState.collectAsState()
    val activeUrl = connectionState.activeUrl

    if (activeUrl == null) {
        ConnectionScreen(
            viewModel = connectionViewModel,
            showCancel = false,
            onConnected = {},
        )
    } else {
        // Keying the whole authenticated app on the active URL forces every
        // screen's ViewModel to be rebuilt against the new repository when the
        // person switches servers, instead of quietly polling the old one.
        key(activeUrl) {
            val repository = remember(activeUrl) { MonitorRepository(activeUrl) }
            MainScaffold(repository, connectionViewModel, activeUrl)
        }
    }
}

@OptIn(ExperimentalMaterial3Api::class)
@Composable
private fun MainScaffold(
    repository: MonitorRepository,
    connectionViewModel: ConnectionViewModel,
    activeUrl: String,
) {
    val navController = rememberNavController()
    // Not rememberSaveable: ServerView carries no Saver, and losing the
    // selection across process death just falls back to the server list,
    // which the ServerDetail branch below already handles.
    var selectedServer by remember(activeUrl) { mutableStateOf<ServerView?>(null) }
    var showSwitchServer by remember { mutableStateOf(false) }

    if (showSwitchServer) {
        Scaffold { padding ->
            Box(Modifier.padding(padding)) {
                ConnectionScreen(
                    viewModel = connectionViewModel,
                    showCancel = true,
                    onConnected = { showSwitchServer = false },
                    onCancel = { showSwitchServer = false },
                )
            }
        }
        return
    }

    val backStackEntry by navController.currentBackStackEntryAsState()
    val currentRoute = backStackEntry?.destination?.route

    Scaffold(
        topBar = {
            TopAppBar(
                title = { Text(titleFor(currentRoute, selectedServer)) },
                navigationIcon = {
                    if (currentRoute == Routes.ServerDetail) {
                        IconButton(onClick = { navController.popBackStack() }) {
                            Icon(Icons.AutoMirrored.Filled.ArrowBack, contentDescription = "Back")
                        }
                    }
                },
            )
        },
        bottomBar = {
            if (currentRoute != Routes.ServerDetail) {
                NavigationBar {
                    tabs.forEach { tab ->
                        NavigationBarItem(
                            selected = currentRoute == tab.route,
                            onClick = {
                                if (tab.route == Routes.Settings) {
                                    showSwitchServer = true
                                } else {
                                    navController.navigate(tab.route) {
                                        popUpTo(navController.graph.findStartDestination().id) { saveState = true }
                                        launchSingleTop = true
                                        restoreState = true
                                    }
                                }
                            },
                            icon = { Icon(tab.icon, contentDescription = tab.label) },
                            label = { Text(tab.label) },
                        )
                    }
                }
            }
        },
    ) { padding ->
        NavHost(
            navController = navController,
            startDestination = Routes.Servers,
            modifier = Modifier.padding(padding),
        ) {
            composable(Routes.Servers) {
                val vm = rememberViewModel { ServersViewModel(repository) }
                ServersScreen(vm, onOpenServer = { server ->
                    selectedServer = server
                    navController.navigate(Routes.ServerDetail)
                })
            }
            composable(Routes.ServerDetail) {
                val server = selectedServer
                if (server == null) {
                    LaunchedEffect(Unit) { navController.popBackStack() }
                } else {
                    val vm = rememberViewModel(key = server.id) { ServerDetailViewModel(repository, server.id) }
                    ServerDetailScreen(vm)
                }
            }
            composable(Routes.Matrix) {
                val vm = rememberViewModel { MatrixViewModel(repository) }
                MatrixScreen(vm)
            }
            composable(Routes.Peers) {
                val vm = rememberViewModel { PeersViewModel(repository) }
                PeersScreen(vm)
            }
        }
    }
}

private fun titleFor(route: String?, selectedServer: ServerView?): String = when (route) {
    Routes.ServerDetail -> selectedServer?.name ?: "Server"
    Routes.Matrix -> "Availability"
    Routes.Peers -> "Mesh peers"
    else -> "m0nit0r"
}
