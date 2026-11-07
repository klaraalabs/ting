package com.klaraalabs.ting.push

import android.Manifest
import android.app.NotificationManager
import android.content.Context
import android.content.pm.PackageManager
import android.os.Build
import androidx.activity.result.ActivityResultLauncher
import androidx.core.app.NotificationManagerCompat
import androidx.core.content.ContextCompat
import com.google.firebase.messaging.FirebaseMessaging
import com.klaraalabs.ting.*
import com.klaraalabs.ting.installations.*
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.suspendCancellableCoroutine
import kotlinx.coroutines.withContext
import kotlin.coroutines.resume

/** Host configures Firebase and creates the channel used by its FCM default-channel manifest metadata. */
class AndroidPushRegistration(
    context: Context,
    private val channelId: String,
    private val messaging: FirebaseMessaging = FirebaseMessaging.getInstance(),
) : PushRegistrationProvider {
    private val context = context.applicationContext
    override suspend fun permission(): PushPermission = TingPermissions.status(context, channelId)
    override suspend fun token(): String = suspendCancellableCoroutine { continuation ->
        messaging.token.addOnCompleteListener { task ->
            if (continuation.isActive) {
                if (task.isSuccessful) continuation.resume(task.result)
                else continuation.resumeWith(Result.failure(TingException(TingFailure(FailureKind.PUSH_UNAVAILABLE))))
            }
        }
    }
}

object TingPermissions {
    fun status(context: Context, channelId: String): PushPermission {
        if (Build.VERSION.SDK_INT >= 33 && ContextCompat.checkSelfPermission(context, Manifest.permission.POST_NOTIFICATIONS) != PackageManager.PERMISSION_GRANTED)
            return PushPermission.RUNTIME_PERMISSION_REQUIRED
        if (!NotificationManagerCompat.from(context).areNotificationsEnabled()) return PushPermission.NOTIFICATIONS_DISABLED
        if (Build.VERSION.SDK_INT >= 26) {
            val channel = context.getSystemService(NotificationManager::class.java).getNotificationChannel(channelId)
                ?: return PushPermission.CHANNEL_MISSING
            if (channel.importance == NotificationManager.IMPORTANCE_NONE) return PushPermission.CHANNEL_DISABLED
        }
        return PushPermission.ALLOWED
    }

    /** Explicit UI action, on Main. Register the launcher in the host before the Activity starts. */
    fun requestPermission(launcher: ActivityResultLauncher<String>) {
        if (Build.VERSION.SDK_INT >= 33) launcher.launch(Manifest.permission.POST_NOTIFICATIONS)
    }
}

/** Construct off Main; no prompt or automatic restoration of the journal's previous user. */
suspend fun TingClient.Companion.create(
    context: Context,
    configuration: TingConfiguration,
    identityTokenProvider: IdentityTokenProvider,
    messaging: FirebaseMessaging = FirebaseMessaging.getInstance(),
): TingClient = withContext(Dispatchers.IO) {
    TingClient(configuration, identityTokenProvider, KeystoreInstallationStorage(context),
        AndroidPushRegistration(context, configuration.notificationChannelId, messaging))
}
