package com.m0n5ter.monitor.data.network

import com.m0n5ter.monitor.data.model.AddPeerRequest
import com.m0n5ter.monitor.data.model.HistoryEntry
import com.m0n5ter.monitor.data.model.MatrixView
import com.m0n5ter.monitor.data.model.Metric
import com.m0n5ter.monitor.data.model.PeerView
import com.m0n5ter.monitor.data.model.ServerView
import retrofit2.http.Body
import retrofit2.http.DELETE
import retrofit2.http.GET
import retrofit2.http.POST
import retrofit2.http.Path
import retrofit2.http.Query

/** Mirrors the read side of internal/api/api.go's router, plus peer management. */
interface ApiService {

    @GET("api/servers")
    suspend fun getServers(): List<ServerView>

    @GET("api/servers/{id}/metrics")
    suspend fun getServerMetrics(
        @Path("id") id: String,
        @Query("hours") hours: Int,
    ): List<Metric>

    @GET("api/availability/matrix")
    suspend fun getAvailabilityMatrix(): MatrixView

    @GET("api/availability/history/{fromId}/{toId}")
    suspend fun getAvailabilityHistory(
        @Path("fromId") fromId: String,
        @Path("toId") toId: String,
        @Query("hours") hours: Int,
    ): List<HistoryEntry>

    @GET("api/peers")
    suspend fun getPeers(): List<PeerView>

    @POST("api/peers")
    suspend fun addPeer(@Body request: AddPeerRequest): PeerView

    @DELETE("api/peers/{id}")
    suspend fun removePeer(@Path("id") id: String)
}
