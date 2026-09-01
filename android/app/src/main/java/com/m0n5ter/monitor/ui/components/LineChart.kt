package com.m0n5ter.monitor.ui.components

import androidx.compose.foundation.Canvas
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.material3.MaterialTheme
import androidx.compose.runtime.Composable
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.Path
import androidx.compose.ui.graphics.drawscope.Stroke
import androidx.compose.ui.unit.dp

/**
 * A minimal line chart for one series, drawn with plain Canvas so the app
 * carries no charting dependency. Values are plotted against [minValue] and
 * [maxValue]; points outside that range are clamped rather than rescaling the
 * axis, which keeps the y-scale stable (e.g. always 0-100 for a percentage)
 * as new samples arrive.
 */
@Composable
fun LineChart(
    values: List<Float>,
    modifier: Modifier = Modifier,
    minValue: Float = 0f,
    maxValue: Float = 100f,
    lineColor: Color = MaterialTheme.colorScheme.primary,
    fillColor: Color = lineColor.copy(alpha = 0.15f),
) {
    Canvas(modifier = modifier.fillMaxWidth().height(120.dp)) {
        if (values.size < 2) return@Canvas
        val range = (maxValue - minValue).takeIf { it > 0f } ?: 1f
        val stepX = size.width / (values.size - 1)

        fun yFor(v: Float): Float {
            val clamped = v.coerceIn(minValue, maxValue)
            return size.height - ((clamped - minValue) / range) * size.height
        }

        val linePath = Path()
        val fillPath = Path()
        values.forEachIndexed { index, value ->
            val x = index * stepX
            val y = yFor(value)
            if (index == 0) {
                linePath.moveTo(x, y)
                fillPath.moveTo(x, size.height)
                fillPath.lineTo(x, y)
            } else {
                linePath.lineTo(x, y)
                fillPath.lineTo(x, y)
            }
        }
        fillPath.lineTo((values.size - 1) * stepX, size.height)
        fillPath.close()

        drawPath(fillPath, color = fillColor)
        drawPath(linePath, color = lineColor, style = Stroke(width = 3f))
    }
}
