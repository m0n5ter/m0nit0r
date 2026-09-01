package com.m0n5ter.monitor.ui.components

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.size
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.unit.Dp
import androidx.compose.ui.unit.dp
import com.m0n5ter.monitor.ui.theme.StatusDown
import com.m0n5ter.monitor.ui.theme.StatusUp
import com.m0n5ter.monitor.ui.theme.StatusWarn

/** A small filled circle indicating up/down, matching the dashboard's own status dots. */
@Composable
fun StatusDot(isUp: Boolean, modifier: Modifier = Modifier, size: Dp = 10.dp) {
    val color = if (isUp) StatusUp else StatusDown
    Canvas(modifier = modifier.size(size)) {
        drawCircle(color = color)
    }
}

/**
 * Amber/red thresholds for a gauge, shared by percentage bars (default
 * 70/90) and, with explicit bounds, the temperature readings whose warn/danger
 * points README documents as 70/85 °C for processors and 45/55 °C for drives.
 */
fun colorForPercent(value: Double, warn: Double = 70.0, danger: Double = 90.0): Color = when {
    value >= danger -> StatusDown
    value >= warn -> StatusWarn
    else -> StatusUp
}
