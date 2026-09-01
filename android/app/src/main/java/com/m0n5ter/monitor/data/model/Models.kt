package com.m0n5ter.monitor.data.model

import kotlinx.serialization.Serializable

/**
 * Wire types for m0nit0r's JSON API (see internal/model and internal/api on
 * the server). Timestamps arrive as ISO 8601 with no zone suffix - the
 * server always means UTC - so they are kept as raw strings here and parsed
 * on demand by [com.m0n5ter.monitor.util.parseServerTime].
 */

@Serializable
data class Disk(
    val name: String,
    val totalGb: Double,
    val usedGb: Double,
    val freeGb: Double,
    val usagePercent: Double,
    val tempC: Double? = null,
)

@Serializable
data class Metric(
    val timestamp: String,
    val cpuPercent: Double,
    val cpuTempC: Double? = null,
    val memoryPercent: Double,
    val memoryTotalMb: Double,
    val memoryUsedMb: Double,
    val uptimeSeconds: Double,
    val disks: List<Disk> = emptyList(),
)

@Serializable
data class ServerView(
    val id: String,
    val name: String,
    val location: String,
    val url: String,
    val isSelf: Boolean,
    val lastSeen: String,
    val isOnline: Boolean,
    val latest: Metric? = null,
)

@Serializable
data class MatrixEntry(
    val fromServerId: String,
    val toServerId: String,
    val availabilityPercent: Double,
    val lastCheck: String,
    val lastLatencyMs: Double? = null,
    val isAvailable: Boolean,
)

@Serializable
data class HistoryEntry(
    val timestamp: String,
    val isAvailable: Boolean,
    val latencyMs: Double? = null,
)

@Serializable
data class PeerView(
    val id: String,
    val name: String,
    val location: String,
    val url: String,
    val lastSeen: String,
)

@Serializable
data class AddPeerRequest(val url: String)

@Serializable
data class HealthResponse(
    val serverId: String,
    val serverName: String,
    val location: String,
    val alerts: Boolean,
    val timestamp: String,
)
