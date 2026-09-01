package com.m0n5ter.monitor.util

import java.time.Instant
import java.time.LocalDateTime
import java.time.ZoneId
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.time.format.DateTimeFormatterBuilder
import java.time.temporal.ChronoField
import java.time.temporal.ChronoUnit

// Matches model.jsonLayout on the server: ISO 8601 with no zone suffix and a
// variable-length fractional second, meaning UTC by convention. A plain
// LocalDateTime parse handles both a bare second and one with a fraction.
private val serverTimeFormat: DateTimeFormatter = DateTimeFormatterBuilder()
    .appendPattern("yyyy-MM-dd'T'HH:mm:ss")
    .appendFraction(ChronoField.NANO_OF_SECOND, 0, 9, true)
    .toFormatter()

/** Parses a timestamp the server sent, returning epoch-zero if it is blank or malformed. */
fun parseServerTime(value: String?): Instant {
    if (value.isNullOrBlank()) return Instant.EPOCH
    return try {
        LocalDateTime.parse(value, serverTimeFormat).toInstant(ZoneOffset.UTC)
    } catch (_: Exception) {
        try {
            Instant.parse(value)
        } catch (_: Exception) {
            Instant.EPOCH
        }
    }
}

private val relativeUnits = listOf(
    86400L * 365 to "y",
    86400L * 30 to "mo",
    86400L to "d",
    3600L to "h",
    60L to "m",
)

/** Renders how long ago [instant] was, e.g. "3m ago", "just now". */
fun Instant.relativeToNow(now: Instant = Instant.now()): String {
    if (this == Instant.EPOCH) return "never"
    val seconds = ChronoUnit.SECONDS.between(this, now).coerceAtLeast(0)
    if (seconds < 5) return "just now"
    for ((unitSeconds, label) in relativeUnits) {
        if (seconds >= unitSeconds) return "${seconds / unitSeconds}$label ago"
    }
    return "${seconds}s ago"
}

/** Renders a duration in seconds as e.g. "3d 4h", "5h 12m", "42m". */
fun formatUptime(totalSeconds: Double): String {
    var seconds = totalSeconds.toLong().coerceAtLeast(0)
    val days = seconds / 86400; seconds %= 86400
    val hours = seconds / 3600; seconds %= 3600
    val minutes = seconds / 60
    return when {
        days > 0 -> "${days}d ${hours}h"
        hours > 0 -> "${hours}h ${minutes}m"
        else -> "${minutes}m"
    }
}

fun Instant.toLocalTimeLabel(): String =
    DateTimeFormatter.ofPattern("HH:mm").withZone(ZoneId.systemDefault()).format(this)

fun Instant.toLocalDateTimeLabel(): String =
    DateTimeFormatter.ofPattern("MMM d, HH:mm").withZone(ZoneId.systemDefault()).format(this)
