package com.m0n5ter.monitor

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import com.m0n5ter.monitor.ui.MonitorApp
import com.m0n5ter.monitor.ui.theme.MonitorTheme

class MainActivity : ComponentActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        enableEdgeToEdge()
        setContent {
            MonitorTheme {
                MonitorApp()
            }
        }
    }
}
