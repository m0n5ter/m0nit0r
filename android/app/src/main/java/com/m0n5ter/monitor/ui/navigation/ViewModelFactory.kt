package com.m0n5ter.monitor.ui.navigation

import androidx.compose.runtime.Composable
import androidx.lifecycle.ViewModel
import androidx.lifecycle.viewmodel.compose.viewModel
import androidx.lifecycle.viewmodel.initializer
import androidx.lifecycle.viewmodel.viewModelFactory

/**
 * A ViewModel is built from a repository bound to whichever server is
 * currently active, so it cannot rely on a no-arg constructor the default
 * factory could reflect into - this wraps the small viewModelFactory { ... }
 * builder so each screen can just hand over a constructor lambda.
 */
@Composable
inline fun <reified VM : ViewModel> rememberViewModel(key: String? = null, crossinline create: () -> VM): VM =
    viewModel(
        key = key,
        factory = viewModelFactory { initializer { create() } },
    )
