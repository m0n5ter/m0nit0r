package com.m0n5ter.monitor.data.repository

import com.m0n5ter.monitor.data.model.AddPeerRequest
import com.m0n5ter.monitor.data.model.HistoryEntry
import com.m0n5ter.monitor.data.model.MatrixEntry
import com.m0n5ter.monitor.data.model.Metric
import com.m0n5ter.monitor.data.model.PeerView
import com.m0n5ter.monitor.data.model.ServerView
import com.m0n5ter.monitor.data.network.ApiClientFactory

/**
 * Thin wrapper around [com.m0n5ter.monitor.data.network.ApiService] bound to
 * one node's base URL. Any node in the mesh answers for the whole fleet, so
 * this is the only network dependency the rest of the app needs.
 */
class MonitorRepository(baseUrl: String) {
    private val api = ApiClientFactory.forBaseUrl(baseUrl)

    suspend fun servers(): List<ServerView> = api.getServers()

    suspend fun serverMetrics(serverId: String, hours: Int): List<Metric> =
        api.getServerMetrics(serverId, hours)

    suspend fun availabilityMatrix(): List<MatrixEntry> = api.getAvailabilityMatrix()

    suspend fun availabilityHistory(fromId: String, toId: String, hours: Int): List<HistoryEntry> =
        api.getAvailabilityHistory(fromId, toId, hours)

    suspend fun peers(): List<PeerView> = api.getPeers()

    suspend fun addPeer(url: String): PeerView = api.addPeer(AddPeerRequest(url))

    suspend fun removePeer(id: String) = api.removePeer(id)
}
