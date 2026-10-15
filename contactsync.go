package main

// Refresh existing contact profiles before slower per-room chat sync. This also
// updates group participants whose old native chat is no longer available.
// Contact records never create new rooms or ghost identities here.
func (br *IMBridge) syncBlueBubblesContactProfiles() {
	if br.Config.IMessage.Platform != "bluebubbles" {
		return
	}
	matched := 0
	var contacts []*Puppet
	for _, puppet := range br.GetAllPuppets() {
		if puppet == nil {
			continue
		}
		contact, err := br.IM.GetContactInfo(puppet.ID)
		if err != nil || contact == nil {
			continue
		}
		puppet.SyncWithContact(contact)
		contacts = append(contacts, puppet)
		matched++
	}
	br.ZLog.Info().Int("matched_profiles", matched).Msg("BlueBubbles contact profile sync complete")
	br.syncBlueBubblesGroupProfiles(contacts)
}
