package com.klaraalabs.ting.installations

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import android.os.UserManager
import android.system.Os
import com.klaraalabs.ting.*
import java.io.File
import java.io.FileNotFoundException
import java.io.FileOutputStream
import java.security.KeyStore
import java.security.MessageDigest
import javax.crypto.Cipher
import javax.crypto.KeyGenerator
import javax.crypto.SecretKey
import javax.crypto.spec.GCMParameterSpec

/** AES-GCM journal in credential-protected, backup-excluded storage. Never deletes unreadable state. */
class KeystoreInstallationStorage(context: Context) : InstallationStorage {
    private val context = context.applicationContext

    init { if (context.isDeviceProtectedStorage) fail(FailureKind.INVALID_CONFIGURATION) }

    private fun digest(namespace: String): String = MessageDigest.getInstance("SHA-256")
        .digest(namespace.toByteArray()).joinToString("") { "%02x".format(it) }
    private fun file(namespace: String) = File(context.noBackupFilesDir, "ting/${digest(namespace)}.journal")
    private fun alias(namespace: String) = "com.klaraalabs.ting.${digest(namespace)}"

    private fun key(namespace: String, create: Boolean): SecretKey {
        val store = KeyStore.getInstance("AndroidKeyStore").apply { load(null) }
        val existing = store.getKey(alias(namespace), null)
        if (existing is SecretKey) return existing
        // A lost key with existing ciphertext is a recovery problem, never a new installation.
        if (!create) fail(FailureKind.STORAGE_UNAVAILABLE)
        return KeyGenerator.getInstance(KeyProperties.KEY_ALGORITHM_AES, "AndroidKeyStore").apply {
            init(KeyGenParameterSpec.Builder(alias(namespace), KeyProperties.PURPOSE_ENCRYPT or KeyProperties.PURPOSE_DECRYPT)
                .setKeySize(256).setBlockModes(KeyProperties.BLOCK_MODE_GCM)
                .setEncryptionPaddings(KeyProperties.ENCRYPTION_PADDING_NONE)
                .setRandomizedEncryptionRequired(true).build())
        }.generateKey()
    }

    override fun load(namespace: String): ByteArray? {
        if (!context.getSystemService(UserManager::class.java).isUserUnlocked) fail(FailureKind.STORAGE_UNAVAILABLE)
        val file = file(namespace)
        val bytes = try { file.readBytes() } catch (_: FileNotFoundException) {
            if (!file.exists() && (file.parentFile?.exists() == false || file.parentFile?.canRead() == true)) return null
            fail(FailureKind.STORAGE_UNAVAILABLE)
        } catch (_: Exception) { fail(FailureKind.STORAGE_UNAVAILABLE) }
        if (bytes.size < 30 || bytes[0] != 1.toByte()) fail(FailureKind.CORRUPT_JOURNAL)
        return try {
            val cipher = Cipher.getInstance("AES/GCM/NoPadding")
            cipher.init(Cipher.DECRYPT_MODE, key(namespace, false), GCMParameterSpec(128, bytes.copyOfRange(1, 13)))
            cipher.updateAAD(namespace.toByteArray())
            cipher.doFinal(bytes.copyOfRange(13, bytes.size))
        } catch (error: TingException) { throw error }
        catch (_: Exception) { fail(FailureKind.STORAGE_UNAVAILABLE) }
    }

    override fun save(namespace: String, value: ByteArray) {
        if (!context.getSystemService(UserManager::class.java).isUserUnlocked) fail(FailureKind.STORAGE_UNAVAILABLE)
        val target = file(namespace)
        var temp: File? = null
        try {
            val directory = target.parentFile!!
            if (!directory.isDirectory && !directory.mkdirs()) fail(FailureKind.STORAGE_UNAVAILABLE)
            val cipher = Cipher.getInstance("AES/GCM/NoPadding")
            cipher.init(Cipher.ENCRYPT_MODE, key(namespace, create = !target.exists()))
            cipher.updateAAD(namespace.toByteArray())
            val encrypted = byteArrayOf(1) + cipher.iv + cipher.doFinal(value)
            temp = File.createTempFile("journal-", ".tmp", directory)
            FileOutputStream(temp).use { stream -> stream.write(encrypted); stream.fd.sync() }
            // Same-directory POSIX rename atomically replaces the previous complete snapshot and throws on failure.
            Os.rename(temp.absolutePath, target.absolutePath)
        } catch (error: TingException) { throw error }
        catch (_: Exception) { fail(FailureKind.STORAGE_UNAVAILABLE) }
        finally { temp?.delete() }
    }
}
